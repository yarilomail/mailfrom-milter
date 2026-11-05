// Copyright (c) 2026 The mailfrom-milter Authors. All rights reserved.
// Use of this source code is governed by a GNU AGPLv3 style
// license that can be found in the LICENSE file.

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// extractDomain
// ---------------------------------------------------------------------------

func TestExtractDomain(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"user@example.com", "example.com"},
		{"USER@EXAMPLE.COM", "example.com"},
		{"<user@example.com>", "example.com"},
		{" user@example.com ", "example.com"},
		{"user@sub.example.com", "sub.example.com"},
		{"useronly", ""},
		{"user@", ""},
		{"", ""},
		{"@example.com", "example.com"},
	}
	for _, c := range cases {
		got := extractDomain(c.input)
		if got != c.want {
			t.Errorf("extractDomain(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// extractEmailFromHeader
// ---------------------------------------------------------------------------

func TestExtractEmailFromHeader(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"CEO <ceo@victim.com>", "ceo@victim.com"},
		{"<ceo@victim.com>", "ceo@victim.com"},
		{"ceo@victim.com", "ceo@victim.com"},
		{"CEO <CEO@VICTIM.COM>", "ceo@victim.com"},
		{" John Doe <john@example.com> ", "john@example.com"},
		{"\"Doe, John\" <john@example.com>", "john@example.com"},
		{"", ""},
	}
	for _, c := range cases {
		got := extractEmailFromHeader(c.input)
		if got != c.want {
			t.Errorf("extractEmailFromHeader(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// extractDomainFromHeader
// ---------------------------------------------------------------------------

func TestExtractDomainFromHeader(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"CEO <ceo@victim.com>", "victim.com"},
		{"attacker@attacker.com", "attacker.com"},
		{"No Domain", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := extractDomainFromHeader(c.input)
		if got != c.want {
			t.Errorf("extractDomainFromHeader(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// loadConfig
// ---------------------------------------------------------------------------

func TestLoadConfig_Defaults(t *testing.T) {
	_ = os.Unsetenv("LISTEN_ADDR")
	_ = os.Unsetenv("METRICS_ADDR")
	_ = os.Unsetenv("MF_ACTION")
	_ = os.Unsetenv("REJECT_CODE")

	c := loadConfig()

	if c.listenAddr != defaultListenAddr {
		t.Errorf("listenAddr = %q, want %q", c.listenAddr, defaultListenAddr)
	}
	if c.metricsAddr != defaultMetricsAddr {
		t.Errorf("metricsAddr = %q, want %q", c.metricsAddr, defaultMetricsAddr)
	}
	if c.action != actionReject {
		t.Errorf("action = %q, want %q", c.action, actionReject)
	}
	if c.rejectCode != "421" {
		t.Errorf("rejectCode = %q, want %q", c.rejectCode, "421")
	}
}

func TestLoadConfig_EnvVars(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "127.0.0.1:9999")
	t.Setenv("METRICS_ADDR", "127.0.0.1:9090")
	t.Setenv("MF_ACTION", "discard")
	t.Setenv("REJECT_CODE", "550")

	c := loadConfig()

	if c.listenAddr != "127.0.0.1:9999" {
		t.Errorf("listenAddr = %q, want 127.0.0.1:9999", c.listenAddr)
	}
	if c.metricsAddr != "127.0.0.1:9090" {
		t.Errorf("metricsAddr = %q, want 127.0.0.1:9090", c.metricsAddr)
	}
	if c.action != actionDiscard {
		t.Errorf("action = %q, want %q", c.action, actionDiscard)
	}
	if c.rejectCode != "550" {
		t.Errorf("rejectCode = %q, want 550", c.rejectCode)
	}
}

func TestLoadConfig_InvalidAction(t *testing.T) {
	t.Setenv("MF_ACTION", "invalidaction")
	c := loadConfig()
	if c.action != actionReject {
		t.Errorf("invalid action should fall back to reject, got %q", c.action)
	}
}

func TestLoadConfig_InvalidRejectCode(t *testing.T) {
	for _, bad := range []string{"200", "abc", "99", "6xx"} {
		t.Setenv("REJECT_CODE", bad)
		c := loadConfig()
		if c.rejectCode != "421" {
			t.Errorf("REJECT_CODE=%q should fall back to 421, got %q", bad, c.rejectCode)
		}
	}
}

// ---------------------------------------------------------------------------
// milterSession.runChecks
// ---------------------------------------------------------------------------

func TestRunChecks(t *testing.T) {
	cases := []struct {
		name          string
		envelopeFrom  string
		fromHeader    string
		wantCheckData string
	}{
		{
			name:          "domains match",
			envelopeFrom:  "user@example.com",
			fromHeader:    "User <user@example.com>",
			wantCheckData: checkPass,
		},
		{
			name:          "domains mismatch",
			envelopeFrom:  "user@attacker.com",
			fromHeader:    "CEO <ceo@victim.com>",
			wantCheckData: checkFail,
		},
		{
			name:          "case insensitive match",
			envelopeFrom:  "user@EXAMPLE.COM",
			fromHeader:    "user@example.com",
			wantCheckData: checkPass,
		},
		{
			name:          "empty from header",
			envelopeFrom:  "user@example.com",
			fromHeader:    "",
			wantCheckData: checkFail,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &milterSession{
				envelopeFrom: c.envelopeFrom,
				fromHeader:   extractEmailFromHeader(c.fromHeader),
			}
			s.runChecks()
			if s.flagCheckData != c.wantCheckData {
				t.Errorf("flagCheckData = %q, want %q", s.flagCheckData, c.wantCheckData)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// rejectWith — verify SMTP response format
// ---------------------------------------------------------------------------

func TestRejectWith(t *testing.T) {
	cases := []struct {
		rejectCode string
		mfCode     string
		wantPrefix string
	}{
		{"421", "MFC010001", "421 4.7.1 "},
		{"550", "MFC010002", "550 5.7.1 "},
	}
	for _, c := range cases {
		cfg.rejectCode = c.rejectCode
		resp := rejectWith(c.mfCode)
		msg := string(resp.Response().Data)
		if len(msg) < len(c.wantPrefix) || msg[:len(c.wantPrefix)] != c.wantPrefix {
			t.Errorf("rejectWith(%q, %q) message = %q, want prefix %q", c.rejectCode, c.mfCode, msg, c.wantPrefix)
		}
	}
}

// ---------------------------------------------------------------------------
// Observability server
// ---------------------------------------------------------------------------

func TestObservabilityServer(t *testing.T) {
	var ready atomic.Bool
	ts := httptest.NewServer(newObservabilityMux(&ready))
	defer ts.Close()

	get := func(path string) int {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	t.Run("healthz always 200", func(t *testing.T) {
		if got := get("/healthz"); got != http.StatusOK {
			t.Errorf("GET /healthz = %d, want 200", got)
		}
		ready.Store(true)
		if got := get("/healthz"); got != http.StatusOK {
			t.Errorf("GET /healthz (ready) = %d, want 200", got)
		}
	})

	t.Run("readyz 503 before ready", func(t *testing.T) {
		ready.Store(false)
		if got := get("/readyz"); got != http.StatusServiceUnavailable {
			t.Errorf("GET /readyz (not ready) = %d, want 503", got)
		}
	})

	t.Run("readyz 200 after ready", func(t *testing.T) {
		ready.Store(true)
		if got := get("/readyz"); got != http.StatusOK {
			t.Errorf("GET /readyz (ready) = %d, want 200", got)
		}
	})

	t.Run("metrics returns 200", func(t *testing.T) {
		if got := get("/metrics"); got != http.StatusOK {
			t.Errorf("GET /metrics = %d, want 200", got)
		}
	})
}

// The milter timeouts: 3600 s and 60 s by default, read from the
// environment, and anything but a whole number above zero falls back.
func TestLoadConfig_MilterTimeouts(t *testing.T) {
	c := loadConfig()
	if c.milterRead != time.Hour || c.milterWrite != time.Minute {
		t.Errorf("defaults %v / %v, want 1h / 1m", c.milterRead, c.milterWrite)
	}
	t.Setenv("MILTER_READ_TIMEOUT_SECONDS", "900")
	t.Setenv("MILTER_WRITE_TIMEOUT_SECONDS", "30")
	if c = loadConfig(); c.milterRead != 15*time.Minute || c.milterWrite != 30*time.Second {
		t.Errorf("from the environment %v / %v, want 15m / 30s", c.milterRead, c.milterWrite)
	}
	for _, bad := range []string{"0", "-5", "abc", "1.5"} {
		t.Setenv("MILTER_READ_TIMEOUT_SECONDS", bad)
		if c = loadConfig(); c.milterRead != time.Hour {
			t.Errorf("MILTER_READ_TIMEOUT_SECONDS=%q gave %v, want the default 1h", bad, c.milterRead)
		}
	}
}

// The chart's defaults are the code's and reach the container as the
// variables loadConfig reads.
func TestChartTimeoutDefaultsMatchTheCode(t *testing.T) {
	values, err := os.ReadFile("../../helm/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := os.ReadFile("../../helm/templates/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		value, env string
		want       int
	}{
		{"milterReadTimeoutSeconds", "MILTER_READ_TIMEOUT_SECONDS", defaultMilterRead},
		{"milterWriteTimeoutSeconds", "MILTER_WRITE_TIMEOUT_SECONDS", defaultMilterWrite},
	} {
		m := regexp.MustCompile(`(?m)^` + tc.value + `: (\d+)$`).FindSubmatch(values)
		if m == nil {
			t.Errorf("%s not in values.yaml", tc.value)
			continue
		}
		if string(m[1]) != strconv.Itoa(tc.want) {
			t.Errorf("%s = %s in values.yaml, %d in the code", tc.value, m[1], tc.want)
		}
		wire := regexp.MustCompile(`- name: ` + tc.env + `\n\s+value: "?\{\{ \.Values\.` + tc.value + ` \| quote \}\}"?`)
		if !wire.Match(tmpl) {
			t.Errorf("deployment.yaml does not pass %s as %s", tc.value, tc.env)
		}
	}
}
