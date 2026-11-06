// Copyright (c) 2026 The mailfrom-milter Authors. All rights reserved.
// Use of this source code is governed by a GNU AGPLv3 style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0kaba0hub/go-milter"
)

// fakeOwners is the alias table: address -> goto, as the stand holds it.
type fakeOwners struct {
	rows  map[string]string
	err   error
	calls int
}

func (f *fakeOwners) owners(_ context.Context, addr string) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	var out []string
	for _, g := range strings.Split(f.rows[addr], ",") {
		if g != "" {
			out = append(out, g)
		}
	}
	return out, nil
}

func standOwners() *fakeOwners {
	return &fakeOwners{rows: map[string]string{
		"admin@d00001.test": "u1@d00001.test",
		"team@d00001.test":  "u1@d00001.test,u2@d00001.test,u3@d00001.test",
		"info@d00001.test":  "billing@d00001.test",
		"sales@d00002.test": "u1@d00001.test",
	}}
}

// A login may send as itself or as an address whose goto names it; a goto that
// is another alias is not followed, and nothing else passes.
func TestMayUseFor(t *testing.T) {
	cases := []struct {
		login, addr string
		want        bool
	}{
		{"u1@d00001.test", "u1@d00001.test", true},
		{"u1@d00001.test", "U1@D00001.TEST", true},
		{"u1@d00001.test", "admin@d00001.test", true},
		{"u2@d00001.test", "admin@d00001.test", false},
		{"u2@d00001.test", "team@d00001.test", true},
		{"u9@d00001.test", "team@d00001.test", false},
		{"u6@d00001.test", "info@d00001.test", false},
		{"u1@d00001.test", "sales@d00002.test", true},
		{"u1@d00001.test", "u2@d00001.test", false},
		{"u1@d00001.test", "", false},
	}
	c := newSenderCache(standOwners(), 100, time.Minute, time.Minute, time.Second)
	for _, tc := range cases {
		got, err := mayUseFor(c, tc.login, tc.addr)
		if err != nil || got != tc.want {
			t.Errorf("%s as %q: %v %v, want %v", tc.login, tc.addr, got, err, tc.want)
		}
	}
}

// The cache answers a repeat within its TTL, asks again after it, keeps at
// most size entries, and does not keep an error.
func TestSenderCache(t *testing.T) {
	f := standOwners()
	now := time.Unix(0, 0)
	c := newSenderCache(f, 2, time.Minute, 10*time.Second, time.Second)
	c.now = func() time.Time { return now }
	ask := func(addr string) {
		if _, err := c.owners(addr); err != nil {
			t.Fatal(err)
		}
	}
	ask("admin@d00001.test")
	ask("ADMIN@d00001.test")
	if f.calls != 1 {
		t.Fatalf("a repeat asked the table: %d calls", f.calls)
	}
	ask("nobody@d00001.test")
	now = now.Add(11 * time.Second)
	ask("nobody@d00001.test")
	if f.calls != 3 {
		t.Fatalf("a miss past its TTL was not asked again: %d calls", f.calls)
	}
	ask("team@d00001.test")
	if len(c.items) != 2 {
		t.Fatalf("%d entries, want at most 2", len(c.items))
	}
	ask("admin@d00001.test")
	if f.calls != 5 {
		t.Fatalf("the least recent entry was kept: %d calls", f.calls)
	}

	f.err = errors.New("down")
	if _, err := c.owners("sales@d00002.test"); err == nil {
		t.Fatal("an error was answered")
	}
	f.err = nil
	ask("sales@d00002.test")
	if f.calls != 7 {
		t.Fatalf("an error was cached: %d calls", f.calls)
	}
}

// MailFrom with the alias table: an alias passes check A, another login's
// address fails it, a table error tempfails; without the table the domains
// decide as before.
func TestMailFromSenderCheck(t *testing.T) {
	cfg = config{action: actionReject, rejectCode: "421"}
	defer func() { senders = nil }()
	cases := []struct {
		name     string
		table    *fakeOwners
		login    string
		from     string
		wantResp milter.Response
		wantAuth string
	}{
		{"alias in another domain", standOwners(), "u1@d00001.test", "sales@d00002.test", milter.RespContinue, checkPass},
		{"own domain, other login", standOwners(), "u1@d00001.test", "u2@d00001.test", rejectWith("MFC010001"), checkFail},
		{"table down", &fakeOwners{err: errors.New("down")}, "u1@d00001.test", "sales@d00002.test", milter.RespTempFail, ""},
		{"no table, same domain", nil, "u1@d00001.test", "u2@d00001.test", milter.RespContinue, checkPass},
		{"no table, alias in another domain", nil, "u1@d00001.test", "sales@d00002.test", rejectWith("MFC010001"), checkFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			senders = nil
			if tc.table != nil {
				senders = newSenderCache(tc.table, 10, time.Minute, time.Minute, time.Second)
			}
			s := &milterSession{}
			resp, err := s.MailFrom("<"+tc.from+">", &milter.Modifier{Macros: map[string]string{"{auth_authen}": tc.login}})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(resp, tc.wantResp) {
				t.Errorf("response %v, want %v", resp, tc.wantResp)
			}
			if s.flagCheckAuth != tc.wantAuth {
				t.Errorf("check A %q, want %q", s.flagCheckAuth, tc.wantAuth)
			}
		})
	}
}

// The chart's sender block carries the code's defaults and query.
func TestChartSenderDefaultsMatchTheCode(t *testing.T) {
	values, err := os.ReadFile("../../helm/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key  string
		want string
	}{
		{"cacheSize", strconv.Itoa(defaultSenderCacheSize)},
		{"cacheTTLSeconds", strconv.Itoa(defaultSenderTTL)},
		{"cacheMissTTLSeconds", strconv.Itoa(defaultSenderMissTTL)},
		{"dbTimeoutSeconds", strconv.Itoa(defaultSenderDBTimeout)},
		{"query", strconv.Quote(defaultSenderQuery)},
	} {
		m := regexp.MustCompile(`(?m)^  ` + tc.key + `: (.+)$`).FindSubmatch(values)
		if m == nil || string(m[1]) != tc.want {
			t.Errorf("sender.%s in values.yaml = %q, the code has %s", tc.key, m, tc.want)
		}
	}
}
