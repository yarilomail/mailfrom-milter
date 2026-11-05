// Copyright (c) 2026 The mailfrom-milter Authors. All rights reserved.
// Use of this source code is governed by a GNU AGPLv3 style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0kaba0hub/go-milter"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// ---------------------------------------------------------------------------
// Server helpers
// ---------------------------------------------------------------------------

func startTestServer(t *testing.T, action, rejectCode string) string {
	t.Helper()

	cfg = config{
		listenAddr:  "127.0.0.1:0",
		action:      action,
		rejectCode:  rejectCode,
		milterRead:  time.Minute,
		milterWrite: time.Minute,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := newMilterServer(cfg)

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})

	return ln.Addr().String()
}

// ---------------------------------------------------------------------------
// Log capture
// ---------------------------------------------------------------------------

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	})
	return buf
}

func lastLogEntry(buf *bytes.Buffer) map[string]string {
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			return m
		}
	}
	return nil
}

func assertLog(t *testing.T, entry map[string]string, key, want string) {
	t.Helper()
	if entry == nil {
		t.Fatalf("no log entry found")
	}
	if got := entry[key]; got != want {
		t.Errorf("log[%q] = %q, want %q", key, got, want)
	}
}

func assertNoLog(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	if buf.Len() > 0 {
		t.Errorf("expected no log output, got: %s", buf.String())
	}
}

// counterDelta captures the current value of a counter and returns a function
// that computes how much it increased since the snapshot.
func counterDelta(c prometheus.Counter) func() float64 {
	before := testutil.ToFloat64(c)
	return func() float64 {
		return testutil.ToFloat64(c) - before
	}
}

// msgCounter returns the counter for a specific label combination.
func msgCounter(action, checkAuth, checkData string) prometheus.Counter {
	return metricMessages.WithLabelValues(action, checkAuth, checkData)
}

// ---------------------------------------------------------------------------
// Milter client helper
// ---------------------------------------------------------------------------

type result struct {
	mailAct    *milter.Action
	eohAct     *milter.Action
	modifyActs []milter.ModifyAction
	finalAct   *milter.Action
}

func (r *result) effectiveAction() *milter.Action {
	if r.mailAct != nil && r.mailAct.Code != milter.ActContinue {
		return r.mailAct
	}
	if r.eohAct != nil && r.eohAct.Code != milter.ActContinue {
		return r.eohAct
	}
	return r.finalAct
}

func sendMessage(t *testing.T, addr, authUser, envelopeFrom, fromHeader string) result {
	t.Helper()

	c := milter.NewClientWithOptions("tcp", addr, milter.ClientOptions{
		ActionMask: milter.OptAddHeader,
	})
	sess, err := c.Session()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if err := sess.Macros(milter.CodeConn, "{client_addr}", "127.0.0.1"); err != nil {
		t.Fatalf("macros conn: %v", err)
	}
	if act, err := sess.Conn("localhost", milter.FamilyInet, 25, "127.0.0.1"); err != nil || act.Code != milter.ActContinue {
		t.Fatalf("conn: err=%v act=%v", err, act)
	}
	if act, err := sess.Helo("localhost"); err != nil || act.Code != milter.ActContinue {
		t.Fatalf("helo: err=%v act=%v", err, act)
	}
	if err := sess.Macros(milter.CodeMail, "{auth_authen}", authUser, "{mail_addr}", envelopeFrom); err != nil {
		t.Fatalf("macros mail: %v", err)
	}

	mailAct, err := sess.Mail(envelopeFrom, nil)
	if err != nil {
		t.Fatalf("mail: %v", err)
	}
	if mailAct.Code != milter.ActContinue {
		return result{mailAct: mailAct}
	}

	if act, err := sess.Rcpt("recipient@example.com", nil); err != nil || act.Code != milter.ActContinue {
		t.Fatalf("rcpt: err=%v act=%v", err, act)
	}
	if act, err := sess.HeaderField("From", fromHeader); err != nil || act.Code != milter.ActContinue {
		t.Fatalf("header: err=%v act=%v", err, act)
	}

	eohAct, err := sess.HeaderEnd()
	if err != nil {
		t.Fatalf("header end: %v", err)
	}
	if eohAct.Code != milter.ActContinue {
		return result{mailAct: mailAct, eohAct: eohAct}
	}

	modifyActs, finalAct, err := sess.End()
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	return result{mailAct: mailAct, eohAct: eohAct, modifyActs: modifyActs, finalAct: finalAct}
}

func assertHeader(t *testing.T, acts []milter.ModifyAction, name, wantValue string) {
	t.Helper()
	for _, a := range acts {
		if a.HeaderName == name {
			if a.HeaderValue != wantValue {
				t.Errorf("header %q = %q, want %q", name, a.HeaderValue, wantValue)
			}
			return
		}
	}
	t.Errorf("header %q not found in modify actions", name)
}

// ---------------------------------------------------------------------------
// action: reject
// ---------------------------------------------------------------------------

func TestAction_Reject(t *testing.T) {
	t.Run("unauthenticated session passes through without inspection", func(t *testing.T) {
		addr := startTestServer(t, actionReject, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "", "user@example.com", "User <user@example.com>")

		if r.effectiveAction().Code != milter.ActAccept {
			t.Errorf("got %v, want Accept", r.effectiveAction().Code)
		}
		assertNoLog(t, buf)
	})

	t.Run("auth domain mismatch → reject 421 MFC010001 at MAIL FROM", func(t *testing.T) {
		addr := startTestServer(t, actionReject, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "user@legit.com", "sender@other.com", "sender@other.com")

		act := r.effectiveAction()
		if act.Code != milter.ActReplyCode {
			t.Fatalf("got %v, want ReplyCode", act.Code)
		}
		if act.SMTPCode != 421 {
			t.Errorf("SMTP code %d, want 421", act.SMTPCode)
		}
		if !strings.Contains(act.SMTPText, "MFC010001") {
			t.Errorf("expected MFC010001 in response, got %q", act.SMTPText)
		}
		entry := lastLogEntry(buf)
		assertLog(t, entry, "flag_check_auth", "fail")
		assertLog(t, entry, "return_code", "reject")
	})

	t.Run("spoofed From header → reject 421 MFC010002 at EOH", func(t *testing.T) {
		addr := startTestServer(t, actionReject, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "attacker@attacker.com", "attacker@attacker.com", "CEO <ceo@victim.com>")

		act := r.effectiveAction()
		if act.Code != milter.ActReplyCode {
			t.Fatalf("got %v, want ReplyCode", act.Code)
		}
		if act.SMTPCode != 421 {
			t.Errorf("SMTP code %d, want 421", act.SMTPCode)
		}
		if !strings.Contains(act.SMTPText, "MFC010002") {
			t.Errorf("expected MFC010002 in response, got %q", act.SMTPText)
		}
		entry := lastLogEntry(buf)
		assertLog(t, entry, "flag_check_auth", "pass")
		assertLog(t, entry, "flag_check_data", "fail")
		assertLog(t, entry, "return_code", "reject")
	})

	t.Run("all checks pass → accept and log", func(t *testing.T) {
		addr := startTestServer(t, actionReject, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "user@example.com", "user@example.com", "User <user@example.com>")

		if r.effectiveAction().Code != milter.ActAccept {
			t.Errorf("got %v, want Accept", r.effectiveAction().Code)
		}
		entry := lastLogEntry(buf)
		assertLog(t, entry, "flag_check_auth", "pass")
		assertLog(t, entry, "flag_check_data", "pass")
		assertLog(t, entry, "return_code", "accept")
	})

	t.Run("REJECT_CODE=550 → permanent reject 550", func(t *testing.T) {
		addr := startTestServer(t, actionReject, "550")
		buf := captureLog(t)

		r := sendMessage(t, addr, "attacker@attacker.com", "attacker@attacker.com", "CEO <ceo@victim.com>")

		act := r.effectiveAction()
		if act.Code != milter.ActReplyCode {
			t.Fatalf("got %v, want ReplyCode", act.Code)
		}
		if act.SMTPCode != 550 {
			t.Errorf("SMTP code %d, want 550", act.SMTPCode)
		}
		entry := lastLogEntry(buf)
		assertLog(t, entry, "return_code", "reject")
	})
}

// ---------------------------------------------------------------------------
// action: discard
// ---------------------------------------------------------------------------

func TestAction_Discard(t *testing.T) {
	t.Run("unauthenticated session passes through without inspection", func(t *testing.T) {
		addr := startTestServer(t, actionDiscard, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "", "user@example.com", "User <user@example.com>")

		if r.effectiveAction().Code != milter.ActAccept {
			t.Errorf("got %v, want Accept", r.effectiveAction().Code)
		}
		assertNoLog(t, buf)
	})

	t.Run("auth domain mismatch → silent discard at MAIL FROM", func(t *testing.T) {
		addr := startTestServer(t, actionDiscard, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "user@legit.com", "sender@other.com", "sender@other.com")

		if r.effectiveAction().Code != milter.ActDiscard {
			t.Errorf("got %v, want Discard", r.effectiveAction().Code)
		}
		entry := lastLogEntry(buf)
		assertLog(t, entry, "flag_check_auth", "fail")
		assertLog(t, entry, "return_code", "discard")
	})

	t.Run("spoofed From header → silent discard at EOH", func(t *testing.T) {
		addr := startTestServer(t, actionDiscard, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "attacker@attacker.com", "attacker@attacker.com", "CEO <ceo@victim.com>")

		if r.effectiveAction().Code != milter.ActDiscard {
			t.Errorf("got %v, want Discard", r.effectiveAction().Code)
		}
		entry := lastLogEntry(buf)
		assertLog(t, entry, "flag_check_auth", "pass")
		assertLog(t, entry, "flag_check_data", "fail")
		assertLog(t, entry, "return_code", "discard")
	})

	t.Run("all checks pass → accept and log", func(t *testing.T) {
		addr := startTestServer(t, actionDiscard, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "user@example.com", "user@example.com", "user@example.com")

		if r.effectiveAction().Code != milter.ActAccept {
			t.Errorf("got %v, want Accept", r.effectiveAction().Code)
		}
		entry := lastLogEntry(buf)
		assertLog(t, entry, "flag_check_auth", "pass")
		assertLog(t, entry, "flag_check_data", "pass")
		assertLog(t, entry, "return_code", "accept")
	})
}

// ---------------------------------------------------------------------------
// action: quarantine_header
// ---------------------------------------------------------------------------

func TestAction_QuarantineHeader(t *testing.T) {
	t.Run("unauthenticated session passes through without inspection", func(t *testing.T) {
		addr := startTestServer(t, actionQuarantineHeader, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "", "user@example.com", "User <user@example.com>")

		if r.effectiveAction().Code != milter.ActAccept {
			t.Errorf("got %v, want Accept", r.effectiveAction().Code)
		}
		assertNoLog(t, buf)
	})

	t.Run("auth domain mismatch → accept with X-MF-Quarantine: yes", func(t *testing.T) {
		addr := startTestServer(t, actionQuarantineHeader, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "user@legit.com", "sender@other.com", "CEO <ceo@victim.com>")

		if r.effectiveAction().Code != milter.ActAccept {
			t.Errorf("got %v, want Accept", r.effectiveAction().Code)
		}
		assertHeader(t, r.modifyActs, headerQuarantine, "yes")
		assertHeader(t, r.modifyActs, headerEnvelopeFrom, "sender@other.com")
		entry := lastLogEntry(buf)
		assertLog(t, entry, "flag_check_auth", "fail")
		assertLog(t, entry, "return_code", "accept")
	})

	t.Run("spoofed From header → accept with X-MF-Quarantine: yes + X-MF-* headers", func(t *testing.T) {
		addr := startTestServer(t, actionQuarantineHeader, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "attacker@attacker.com", "attacker@attacker.com", "CEO <ceo@victim.com>")

		if r.effectiveAction().Code != milter.ActAccept {
			t.Errorf("got %v, want Accept", r.effectiveAction().Code)
		}
		assertHeader(t, r.modifyActs, headerQuarantine, "yes")
		assertHeader(t, r.modifyActs, headerEnvelopeFrom, "attacker@attacker.com")
		assertHeader(t, r.modifyActs, headerFrom, "ceo@victim.com")
		entry := lastLogEntry(buf)
		assertLog(t, entry, "flag_check_auth", "pass")
		assertLog(t, entry, "flag_check_data", "fail")
		assertLog(t, entry, "return_code", "accept")
	})

	t.Run("all checks pass → accept with X-MF-Quarantine: no + X-MF-* headers", func(t *testing.T) {
		addr := startTestServer(t, actionQuarantineHeader, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "user@example.com", "user@example.com", "User <user@example.com>")

		if r.effectiveAction().Code != milter.ActAccept {
			t.Errorf("got %v, want Accept", r.effectiveAction().Code)
		}
		assertHeader(t, r.modifyActs, headerQuarantine, "no")
		assertHeader(t, r.modifyActs, headerEnvelopeFrom, "user@example.com")
		assertHeader(t, r.modifyActs, headerFrom, "user@example.com")
		entry := lastLogEntry(buf)
		assertLog(t, entry, "flag_check_auth", "pass")
		assertLog(t, entry, "flag_check_data", "pass")
		assertLog(t, entry, "return_code", "accept")
	})
}

// ---------------------------------------------------------------------------
// action: accept
// ---------------------------------------------------------------------------

func TestAction_Accept(t *testing.T) {
	t.Run("unauthenticated session passes through without inspection", func(t *testing.T) {
		addr := startTestServer(t, actionDunno, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "", "user@example.com", "User <user@example.com>")

		if r.effectiveAction().Code != milter.ActAccept {
			t.Errorf("got %v, want Accept", r.effectiveAction().Code)
		}
		assertNoLog(t, buf)
	})

	t.Run("auth domain mismatch → accept and log violation", func(t *testing.T) {
		addr := startTestServer(t, actionDunno, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "user@legit.com", "sender@other.com", "CEO <ceo@victim.com>")

		if r.effectiveAction().Code != milter.ActAccept {
			t.Errorf("got %v, want Accept", r.effectiveAction().Code)
		}
		entry := lastLogEntry(buf)
		assertLog(t, entry, "flag_check_auth", "fail")
		assertLog(t, entry, "return_code", "accept")
	})

	t.Run("spoofed From header → accept and log violation", func(t *testing.T) {
		addr := startTestServer(t, actionDunno, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "attacker@attacker.com", "attacker@attacker.com", "CEO <ceo@victim.com>")

		if r.effectiveAction().Code != milter.ActAccept {
			t.Errorf("got %v, want Accept", r.effectiveAction().Code)
		}
		entry := lastLogEntry(buf)
		assertLog(t, entry, "flag_check_auth", "pass")
		assertLog(t, entry, "flag_check_data", "fail")
		assertLog(t, entry, "return_code", "accept")
	})

	t.Run("all checks pass → accept and log", func(t *testing.T) {
		addr := startTestServer(t, actionDunno, "421")
		buf := captureLog(t)

		r := sendMessage(t, addr, "user@example.com", "user@example.com", "User <user@example.com>")

		if r.effectiveAction().Code != milter.ActAccept {
			t.Errorf("got %v, want Accept", r.effectiveAction().Code)
		}
		entry := lastLogEntry(buf)
		assertLog(t, entry, "flag_check_auth", "pass")
		assertLog(t, entry, "flag_check_data", "pass")
		assertLog(t, entry, "return_code", "accept")
	})
}

// ---------------------------------------------------------------------------
// Metrics counters
// ---------------------------------------------------------------------------

func TestMetrics(t *testing.T) {
	t.Run("unauthenticated → accept/skip/skip", func(t *testing.T) {
		addr := startTestServer(t, actionReject, "421")
		delta := counterDelta(msgCounter("accept", "skip", "skip"))

		sendMessage(t, addr, "", "user@example.com", "User <user@example.com>")

		if d := delta(); d != 1 {
			t.Errorf("accept/skip/skip delta = %v, want 1", d)
		}
	})

	t.Run("auth mismatch + reject → reject/fail/skip", func(t *testing.T) {
		addr := startTestServer(t, actionReject, "421")
		delta := counterDelta(msgCounter("reject", "fail", "skip"))

		sendMessage(t, addr, "user@legit.com", "sender@other.com", "sender@other.com")

		if d := delta(); d != 1 {
			t.Errorf("reject/fail/skip delta = %v, want 1", d)
		}
	})

	t.Run("auth mismatch + discard → discard/fail/skip", func(t *testing.T) {
		addr := startTestServer(t, actionDiscard, "421")
		delta := counterDelta(msgCounter("discard", "fail", "skip"))

		sendMessage(t, addr, "user@legit.com", "sender@other.com", "sender@other.com")

		if d := delta(); d != 1 {
			t.Errorf("discard/fail/skip delta = %v, want 1", d)
		}
	})

	t.Run("spoofed header + reject → reject/pass/fail", func(t *testing.T) {
		addr := startTestServer(t, actionReject, "421")
		delta := counterDelta(msgCounter("reject", "pass", "fail"))

		sendMessage(t, addr, "attacker@attacker.com", "attacker@attacker.com", "CEO <ceo@victim.com>")

		if d := delta(); d != 1 {
			t.Errorf("reject/pass/fail delta = %v, want 1", d)
		}
	})

	t.Run("spoofed header + discard → discard/pass/fail", func(t *testing.T) {
		addr := startTestServer(t, actionDiscard, "421")
		delta := counterDelta(msgCounter("discard", "pass", "fail"))

		sendMessage(t, addr, "attacker@attacker.com", "attacker@attacker.com", "CEO <ceo@victim.com>")

		if d := delta(); d != 1 {
			t.Errorf("discard/pass/fail delta = %v, want 1", d)
		}
	})

	t.Run("spoofed header + quarantine → quarantine/pass/fail", func(t *testing.T) {
		addr := startTestServer(t, actionQuarantineHeader, "421")
		delta := counterDelta(msgCounter("quarantine", "pass", "fail"))

		sendMessage(t, addr, "attacker@attacker.com", "attacker@attacker.com", "CEO <ceo@victim.com>")

		if d := delta(); d != 1 {
			t.Errorf("quarantine/pass/fail delta = %v, want 1", d)
		}
	})

	t.Run("clean + quarantine → accept/pass/pass", func(t *testing.T) {
		addr := startTestServer(t, actionQuarantineHeader, "421")
		delta := counterDelta(msgCounter("accept", "pass", "pass"))

		sendMessage(t, addr, "user@example.com", "user@example.com", "User <user@example.com>")

		if d := delta(); d != 1 {
			t.Errorf("accept/pass/pass delta = %v, want 1", d)
		}
	})

	t.Run("clean + reject → accept/pass/pass", func(t *testing.T) {
		addr := startTestServer(t, actionReject, "421")
		delta := counterDelta(msgCounter("accept", "pass", "pass"))

		sendMessage(t, addr, "user@example.com", "user@example.com", "User <user@example.com>")

		if d := delta(); d != 1 {
			t.Errorf("accept/pass/pass delta = %v, want 1", d)
		}
	})

	t.Run("connection counter increments per session", func(t *testing.T) {
		addr := startTestServer(t, actionReject, "421")
		delta := counterDelta(metricConnections)

		sendMessage(t, addr, "user@example.com", "user@example.com", "User <user@example.com>")

		if d := delta(); d != 1 {
			t.Errorf("connections delta = %v, want 1", d)
		}
	})
}

// The server main() starts closes a connection that stays silent past
// MILTER_READ_TIMEOUT_SECONDS.
func TestMilterServerClosesASilentConnection(t *testing.T) {
	srv := newMilterServer(config{milterRead: 200 * time.Millisecond, milterWrite: time.Second})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("a silent connection: %v, want it closed by the server", err)
	}
}
