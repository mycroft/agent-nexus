package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
)

// Lazy opens the sessions of a job's lazy servers when the model first
// uses them, once each: a server that fails stays failed for the job.
type Lazy struct {
	clients []*Client
	logger  *slog.Logger

	mu     sync.Mutex
	opened map[string]*lazySession
}

type lazySession struct {
	once sync.Once
	s    *Session
	err  error
}

// NewLazy returns a Lazy over clients, which logs to logger.
func NewLazy(clients []*Client, logger *slog.Logger) *Lazy {
	if logger == nil {
		logger = slog.Default()
	}
	return &Lazy{clients: clients, logger: logger, opened: map[string]*lazySession{}}
}

// Servers returns the configuration of each server, in order.
func (l *Lazy) Servers() []Server {
	out := make([]Server, len(l.clients))
	for i, c := range l.clients {
		out[i] = c.Server
	}
	return out
}

// Session returns the open session of the server name, opening it on
// first use. Why a server failed is logged, not given to the model.
func (l *Lazy) Session(ctx context.Context, name string) (*Session, error) {
	i := slices.IndexFunc(l.clients, func(c *Client) bool { return c.Server.Name == name })
	if i < 0 {
		return nil, fmt.Errorf("there is no MCP server %q", name)
	}
	l.mu.Lock()
	o := l.opened[name]
	if o == nil {
		o = &lazySession{}
		l.opened[name] = o
	}
	l.mu.Unlock()
	o.once.Do(func() {
		if o.s, o.err = l.clients[i].Open(ctx); o.err != nil {
			l.logger.WarnContext(ctx, "MCP server unavailable", "server", name, "error", o.err)
		}
	})
	if o.err != nil {
		return nil, fmt.Errorf("the MCP server %s is unavailable", name)
	}
	return o.s, nil
}

// Close ends the sessions opened.
func (l *Lazy) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for name, o := range l.opened {
		// Waits for an open still running, so o.s is safe to read.
		o.once.Do(func() {})
		if o.s == nil {
			continue
		}
		if err := o.s.Close(); err != nil {
			l.logger.Debug("close an MCP session", "server", name, "error", err)
		}
	}
}
