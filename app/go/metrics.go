// Copyright (c) 2026 The mailfrom-milter Authors. All rights reserved.
// Use of this source code is governed by a GNU AGPLv3 style
// license that can be found in the LICENSE file.

package main

import (
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ---------------------------------------------------------------------------
// Metric definitions
// ---------------------------------------------------------------------------

var (
	// metricMessages counts processed SMTP messages by outcome.
	//
	// Labels:
	//   action     — "accept" | "reject" | "discard" | "quarantine" | "tempfail"
	//   check_auth — "pass" | "fail" | "skip" (skip = unauthenticated or not reached)
	//   check_data — "pass" | "fail" | "skip"
	metricMessages = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mailfrom_messages_total",
		Help: "SMTP messages processed by the milter, labeled by outcome.",
	}, []string{"action", "check_auth", "check_data"})

	// metricSenderLookups counts owner lookups by source: cache, db, error.
	metricSenderLookups = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mailfrom_sender_lookups_total",
		Help: "Owner lookups of MAIL FROM addresses, labeled by where the answer came from.",
	}, []string{"source"})

	// metricConnections counts total SMTP connections accepted by the milter.
	metricConnections = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mailfrom_connections_total",
		Help: "Total SMTP connections accepted by the milter.",
	})
)

// recordMessage increments the messages counter.
// Empty checkAuth / checkData are normalised to "skip".
func recordMessage(action, checkAuth, checkData string) {
	if checkAuth == "" {
		checkAuth = checkSkip
	}
	if checkData == "" {
		checkData = checkSkip
	}
	metricMessages.WithLabelValues(action, checkAuth, checkData).Inc()
}

// ---------------------------------------------------------------------------
// Observability HTTP server
// ---------------------------------------------------------------------------

// newObservabilityMux builds the HTTP mux for health and metrics endpoints.
// Extracted for testability.
func newObservabilityMux(ready *atomic.Bool) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
		}
	})

	mux.Handle("/metrics", promhttp.Handler())

	return mux
}

// startObservability starts the HTTP server for /healthz, /readyz and /metrics.
// Intended to run in a dedicated goroutine; exits the process on failure.
func startObservability(addr string, ready *atomic.Bool) {
	slog.Info("observability listening", "addr", addr)
	if err := http.ListenAndServe(addr, newObservabilityMux(ready)); err != nil {
		slog.Error("observability server failed", "err", err)
		os.Exit(1)
	}
}
