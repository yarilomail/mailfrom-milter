// Copyright (c) 2026 The mailfrom-milter Authors. All rights reserved.
// Use of this source code is governed by a GNU AGPLv3 style
// license that can be found in the LICENSE file.
//
// mailfrom-milter — Postfix milter that enforces From-header / envelope-from
// domain alignment for authenticated SMTP sessions.
//
// Environment variables:
//
//	LISTEN_ADDR   — TCP address to listen on (default: 0.0.0.0:10031)
//	METRICS_ADDR  — TCP address for /healthz, /readyz, /metrics (default: 0.0.0.0:8081)
//	MF_ACTION     — Action on mismatch: reject | discard | quarantine_header | accept
//	                (default: reject)
//	REJECT_CODE   — SMTP reply code for reject action: 421 (temp) or 550 (permanent)
//	                (default: 421)
//	MF_SENDER_ADD     — Set to "yes" to add X-MF-Envelope-From header on accepted
//	                authenticated messages (reject and discard actions only)
//	LOG_LEVEL     — Set to "debug" for verbose per-message logging
package main

import (
	"errors"
	"log/slog"
	"net"
	"net/textproto"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/0kaba0hub/go-milter"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

const (
	actionReject           = "reject"
	actionDiscard          = "discard"
	actionQuarantineHeader = "quarantine_header"
	actionDunno            = "accept"

	defaultListenAddr  = "0.0.0.0:10031"
	defaultMetricsAddr = "0.0.0.0:8081"
	defaultAction      = actionReject
	// Postfix keeps one milter connection for a whole SMTP session and is
	// silent on it while the client sends, so the read timeout outlasts any
	// session; it only frees connections nobody will use again.
	defaultMilterRead  = 3600
	defaultMilterWrite = 60

	headerEnvelopeFrom = "X-MF-Envelope-From"
	headerFrom         = "X-MF-From"
	headerQuarantine   = "X-MF-Quarantine"
)

type config struct {
	listenAddr  string
	metricsAddr string
	action      string
	rejectCode  string
	mfSender    bool
	milterRead  time.Duration
	milterWrite time.Duration
}

func loadConfig() config {
	cfg := config{
		listenAddr:  envOrDefault("LISTEN_ADDR", defaultListenAddr),
		metricsAddr: envOrDefault("METRICS_ADDR", defaultMetricsAddr),
		action:      strings.ToLower(envOrDefault("MF_ACTION", defaultAction)),
		rejectCode:  envOrDefault("REJECT_CODE", "421"),
		mfSender:    strings.ToLower(os.Getenv("MF_SENDER_ADD")) == "yes",
		milterRead:  seconds("MILTER_READ_TIMEOUT_SECONDS", defaultMilterRead),
		milterWrite: seconds("MILTER_WRITE_TIMEOUT_SECONDS", defaultMilterWrite),
	}
	switch cfg.action {
	case actionReject, actionDiscard, actionQuarantineHeader, actionDunno:
	default:
		slog.Warn("unknown MF_ACTION value, falling back to reject", "value", cfg.action)
		cfg.action = actionReject
	}
	if len(cfg.rejectCode) != 3 || (cfg.rejectCode[0] != '4' && cfg.rejectCode[0] != '5') {
		slog.Warn("invalid REJECT_CODE, falling back to 421", "value", cfg.rejectCode)
		cfg.rejectCode = "421"
	}
	return cfg
}

// seconds reads a whole number of seconds above zero; anything else falls
// back to def with a warning, as the other settings do.
func seconds(key string, def int) time.Duration {
	v := envOrDefault(key, strconv.Itoa(def))
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("invalid "+key+", falling back to the default", "value", v, "default", def)
		n = def
	}
	return time.Duration(n) * time.Second
}

// newMilterServer is the milter listener's server. The timeouts bound a
// connection that stops talking; the library also refuses packets over
// 1 MiB, so a peer cannot make it allocate more.
func newMilterServer(c config) *milter.Server {
	return &milter.Server{
		NewMilter:    func() milter.Milter { return &milterSession{} },
		Actions:      milter.OptAddHeader,
		ReadTimeout:  c.milterRead,
		WriteTimeout: c.milterWrite,
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Debug logging
// ---------------------------------------------------------------------------

var debug bool

func debugf(msg string, args ...any) {
	if debug {
		slog.Debug(msg, args...)
	}
}

// ---------------------------------------------------------------------------
// Milter implementation
// ---------------------------------------------------------------------------

var cfg config

const (
	checkPass = "pass"
	checkFail = "fail"
	checkSkip = "skip"
)

// rejectWith builds a reject response using cfg.rejectCode.
func rejectWith(mfCode string) milter.Response {
	class := string(cfg.rejectCode[0])
	msg := cfg.rejectCode + " " + class + ".7.1 Rejected due of violation of policy. Error code: " + mfCode
	return milter.NewResponseStr(byte(milter.ActReplyCode), msg)
}

type milterSession struct {
	// Per-connection
	clientAddr string

	// Per-message (reset on each MAIL FROM)
	envelopeFrom      string
	authUser          string
	fromHeader        string
	flagCheckAuth     string
	flagCheckData     string
	addEnvelopeHeader bool
	queueID           string
}

func (s *milterSession) reset() {
	s.envelopeFrom = ""
	s.authUser = ""
	s.fromHeader = ""
	s.flagCheckAuth = ""
	s.flagCheckData = ""
	s.addEnvelopeHeader = false
	s.queueID = ""
}

func (s *milterSession) Connect(host string, family string, port uint16, addr net.IP, m *milter.Modifier) (milter.Response, error) {
	if addr != nil {
		s.clientAddr = addr.String()
	}
	debugf("connect", "host", host, "addr", s.clientAddr)
	metricConnections.Inc()
	return milter.RespContinue, nil
}

func (s *milterSession) Helo(name string, m *milter.Modifier) (milter.Response, error) {
	return milter.RespContinue, nil
}

func (s *milterSession) MailFrom(from string, m *milter.Modifier) (milter.Response, error) {
	s.reset()
	s.envelopeFrom = strings.Trim(from, "<> \t")
	s.authUser = m.Macros["{auth_authen}"]

	// No SASL auth — inbound delivery, not our concern.
	if s.authUser == "" {
		debugf("unauthenticated, skipping", "envelope_from", s.envelopeFrom)
		recordMessage("accept", checkSkip, checkSkip)
		return milter.RespAccept, nil
	}

	// Check A: authUser domain vs envelopeFrom domain.
	authDomain := extractDomain(s.authUser)
	envDomain := extractDomain(s.envelopeFrom)
	if strings.EqualFold(authDomain, envDomain) {
		s.flagCheckAuth = checkPass
	} else {
		s.flagCheckAuth = checkFail
	}

	debugf("mail from",
		"envelope_from", s.envelopeFrom,
		"auth_user", s.authUser,
		"flag_check_auth", s.flagCheckAuth,
	)

	if s.flagCheckAuth == checkFail {
		switch cfg.action {
		case actionReject:
			s.logResult(actionReject)
			recordMessage("reject", s.flagCheckAuth, checkSkip)
			return rejectWith("MFC010001"), nil
		case actionDiscard:
			s.logResult(actionDiscard)
			recordMessage("discard", s.flagCheckAuth, checkSkip)
			return milter.RespDiscard, nil
		}
	}

	return milter.RespContinue, nil
}

func (s *milterSession) RcptTo(rcptTo string, m *milter.Modifier) (milter.Response, error) {
	return milter.RespContinue, nil
}

// Header captures the email address from the first From: header.
func (s *milterSession) Header(name string, value string, m *milter.Modifier) (milter.Response, error) {
	if strings.EqualFold(name, "From") && s.fromHeader == "" {
		s.fromHeader = extractEmailFromHeader(value)
		debugf("captured From header", "value", s.fromHeader)
	}
	return milter.RespContinue, nil
}

// Headers is called after all message headers are received.
func (s *milterSession) Headers(h textproto.MIMEHeader, m *milter.Modifier) (milter.Response, error) {
	s.queueID = m.Macros["i"]
	switch cfg.action {
	case actionDunno:
		return s.handleDunno()
	case actionQuarantineHeader:
		return s.handleQuarantineHeader()
	case actionReject:
		return s.handleReject()
	case actionDiscard:
		return s.handleDiscard()
	default:
		return milter.RespAccept, nil
	}
}

// runChecks performs check B (envelopeFrom domain vs From: header domain).
func (s *milterSession) runChecks() {
	envDomain := extractDomain(s.envelopeFrom)
	fromDomain := extractDomainFromHeader(s.fromHeader)
	if strings.EqualFold(envDomain, fromDomain) {
		s.flagCheckData = checkPass
	} else {
		s.flagCheckData = checkFail
	}
}

func (s *milterSession) logResult(returnCode string) {
	slog.Info("milter",
		"queue_id", s.queueID,
		"envelope_from", s.envelopeFrom,
		"auth_user", s.authUser,
		"flag_check_auth", s.flagCheckAuth,
		"from_header", s.fromHeader,
		"flag_check_data", s.flagCheckData,
		"return_code", returnCode,
	)
}

func (s *milterSession) addHeaders(m *milter.Modifier, quarantineVal string) {
	if err := m.AddHeader(headerEnvelopeFrom, s.envelopeFrom); err != nil {
		slog.Error("failed to add "+headerEnvelopeFrom, "err", err)
	}
	if err := m.AddHeader(headerFrom, s.fromHeader); err != nil {
		slog.Error("failed to add "+headerFrom, "err", err)
	}
	if err := m.AddHeader(headerQuarantine, quarantineVal); err != nil {
		slog.Error("failed to add "+headerQuarantine, "err", err)
	}
}

func (s *milterSession) handleDunno() (milter.Response, error) {
	s.runChecks()
	s.logResult(actionDunno)
	recordMessage("accept", s.flagCheckAuth, s.flagCheckData)
	return milter.RespAccept, nil
}

func (s *milterSession) handleQuarantineHeader() (milter.Response, error) {
	s.runChecks()
	s.logResult(actionDunno)
	// Metric is recorded in Body() once quarantine value is known.
	return milter.RespContinue, nil
}

func (s *milterSession) handleDiscard() (milter.Response, error) {
	s.runChecks()
	if s.flagCheckData == checkFail {
		s.logResult(actionDiscard)
		recordMessage("discard", s.flagCheckAuth, s.flagCheckData)
		return milter.RespDiscard, nil
	}
	s.logResult(actionDunno)
	recordMessage("accept", s.flagCheckAuth, s.flagCheckData)
	if cfg.mfSender {
		s.addEnvelopeHeader = true
		return milter.RespContinue, nil
	}
	return milter.RespAccept, nil
}

func (s *milterSession) handleReject() (milter.Response, error) {
	s.runChecks()
	if s.flagCheckData == checkFail {
		s.logResult(actionReject)
		recordMessage("reject", s.flagCheckAuth, s.flagCheckData)
		return rejectWith("MFC010002"), nil
	}
	s.logResult(actionDunno)
	recordMessage("accept", s.flagCheckAuth, s.flagCheckData)
	if cfg.mfSender {
		s.addEnvelopeHeader = true
		return milter.RespContinue, nil
	}
	return milter.RespAccept, nil
}

func (s *milterSession) BodyChunk(chunk []byte, m *milter.Modifier) (milter.Response, error) {
	return milter.RespContinue, nil
}

func (s *milterSession) Body(m *milter.Modifier) (milter.Response, error) {
	if cfg.action == actionQuarantineHeader {
		quarantineVal := "no"
		if s.flagCheckAuth == checkFail || s.flagCheckData == checkFail {
			quarantineVal = "yes"
		}
		s.addHeaders(m, quarantineVal)

		metricAction := "accept"
		if quarantineVal == "yes" {
			metricAction = "quarantine"
		}
		recordMessage(metricAction, s.flagCheckAuth, s.flagCheckData)
	}
	if s.addEnvelopeHeader {
		if err := m.AddHeader(headerEnvelopeFrom, s.envelopeFrom); err != nil {
			slog.Error("failed to add "+headerEnvelopeFrom, "err", err)
		}
	}
	return milter.RespAccept, nil
}

func (s *milterSession) Abort(m *milter.Modifier) error {
	s.reset()
	return nil
}

// ---------------------------------------------------------------------------
// Domain extraction helpers
// ---------------------------------------------------------------------------

func extractDomain(addr string) string {
	addr = strings.Trim(addr, "<> \t")
	at := strings.LastIndex(addr, "@")
	if at < 0 || at == len(addr)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(addr[at+1:]))
}

// extractEmailFromHeader extracts the email address from a From: header value.
// Handles: "Display Name <user@domain.com>", "<user@domain.com>", "user@domain.com".
func extractEmailFromHeader(header string) string {
	header = strings.TrimSpace(header)
	start := strings.LastIndex(header, "<")
	end := strings.LastIndex(header, ">")
	if start >= 0 && end > start {
		return strings.ToLower(strings.TrimSpace(header[start+1 : end]))
	}
	return strings.ToLower(strings.TrimSpace(header))
}

func extractDomainFromHeader(header string) string {
	return extractDomain(extractEmailFromHeader(header))
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	debug = strings.ToLower(os.Getenv("LOG_LEVEL")) == "debug"
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg = loadConfig()

	slog.Info("starting mailfrom-milter",
		"listen", cfg.listenAddr,
		"metrics", cfg.metricsAddr,
		"action", cfg.action,
		"mf_sender", cfg.mfSender,
		"read_timeout", cfg.milterRead.String(),
		"write_timeout", cfg.milterWrite.String(),
	)

	server := newMilterServer(cfg)

	ln, err := net.Listen("tcp", cfg.listenAddr)
	if err != nil {
		slog.Error("listen failed", "addr", cfg.listenAddr, "err", err)
		os.Exit(1)
	}
	// Serve and server.Close close it first; this one only reports a double close.
	defer func() { _ = ln.Close() }()

	var readiness atomic.Bool
	readiness.Store(true)

	go startObservability(cfg.metricsAddr, &readiness)

	go func() {
		// After server.Close, Serve returns ErrServerClosed: a clean stop, not a failure.
		if err := server.Serve(ln); err != nil && !errors.Is(err, milter.ErrServerClosed) {
			slog.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}()

	slog.Info("milter ready", "addr", cfg.listenAddr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	slog.Info("shutting down")
	readiness.Store(false)
	if err := server.Close(); err != nil {
		slog.Warn("shutdown: closing the listener", "err", err)
	}
}
