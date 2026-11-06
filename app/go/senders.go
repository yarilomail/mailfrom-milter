// Copyright (c) 2026 The mailfrom-milter Authors. All rights reserved.
// Use of this source code is governed by a GNU AGPLv3 style
// license that can be found in the LICENSE file.

package main

import (
	"container/list"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// defaultSenderQuery returns the goto list of an active alias row: a login
// named there may send as the address. A catch-all row (@domain) is never
// asked for, and a goto that is another alias is not followed.
const defaultSenderQuery = "SELECT goto FROM alias WHERE address = ? AND active = 1"

// ownerLookup answers the logins an address belongs to.
type ownerLookup interface {
	owners(ctx context.Context, addr string) ([]string, error)
}

// sqlOwners reads owners with one query; goto is a comma-separated list.
type sqlOwners struct {
	db    *sql.DB
	query string
}

func (s *sqlOwners) owners(ctx context.Context, addr string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.query, addr)
	if err != nil {
		return nil, fmt.Errorf("senders: query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var gotoList string
		if err := rows.Scan(&gotoList); err != nil {
			return nil, fmt.Errorf("senders: scan: %w", err)
		}
		for _, g := range strings.Split(gotoList, ",") {
			if g = strings.TrimSpace(g); g != "" {
				out = append(out, g)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("senders: rows: %w", err)
	}
	return out, nil
}

// senderCache holds owner lists in a bounded LRU: a found address for ttl, an
// address with no row for missTTL.
type senderCache struct {
	next    ownerLookup
	size    int
	ttl     time.Duration
	missTTL time.Duration
	timeout time.Duration
	now     func() time.Time

	mu    sync.Mutex
	order *list.List
	items map[string]*list.Element
}

type cacheEntry struct {
	addr    string
	owners  []string
	expires time.Time
}

func newSenderCache(next ownerLookup, size int, ttl, missTTL, timeout time.Duration) *senderCache {
	return &senderCache{next: next, size: size, ttl: ttl, missTTL: missTTL, timeout: timeout,
		now: time.Now, order: list.New(), items: map[string]*list.Element{}}
}

func (c *senderCache) owners(addr string) ([]string, error) {
	key := strings.ToLower(addr)
	c.mu.Lock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*cacheEntry)
		if c.now().Before(e.expires) {
			c.order.MoveToFront(el)
			c.mu.Unlock()
			metricSenderLookups.WithLabelValues("cache").Inc()
			return e.owners, nil
		}
		c.order.Remove(el)
		delete(c.items, key)
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	owners, err := c.next.owners(ctx, key)
	if err != nil {
		metricSenderLookups.WithLabelValues("error").Inc()
		return nil, err
	}
	metricSenderLookups.WithLabelValues("db").Inc()
	ttl := c.ttl
	if len(owners) == 0 {
		ttl = c.missTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.Remove(el)
	}
	c.items[key] = c.order.PushFront(&cacheEntry{addr: key, owners: owners, expires: c.now().Add(ttl)})
	for c.order.Len() > c.size {
		old := c.order.Back()
		c.order.Remove(old)
		delete(c.items, old.Value.(*cacheEntry).addr)
	}
	return owners, nil
}

// errNoSenders marks a deployment without the owner table: the domain check
// applies instead.
var errNoSenders = errors.New("senders: not configured")

// mayUseFor reports whether login may send as addr: addr is the login itself,
// or the login is one of addr's owners.
func mayUseFor(c *senderCache, login, addr string) (bool, error) {
	if strings.EqualFold(login, addr) {
		return true, nil
	}
	if c == nil {
		return false, errNoSenders
	}
	owners, err := c.owners(addr)
	if err != nil {
		return false, err
	}
	for _, o := range owners {
		if strings.EqualFold(o, login) {
			return true, nil
		}
	}
	return false, nil
}
