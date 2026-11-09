package jmapmemserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"git.sr.ht/~rockorager/go-jmap"
)

type subscriber struct {
	wake chan struct{} // buffered 1: wakes coalesce
	drop chan struct{}
}

// pushTypes are the event types the server reports.
var pushTypes = []string{"Mailbox", "Email", "EmailDelivery"}

// wakeSubscribers runs with the lock held.
func (s *Server) wakeSubscribers() {
	for sub := range s.subs {
		select {
		case sub.wake <- struct{}{}:
		default:
		}
	}
}

// DropEventSources closes every open EventSource connection, as a server
// restart or a proxy timeout would. Clients may reconnect.
func (s *Server) DropEventSources() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		close(sub.drop)
		delete(s.subs, sub)
	}
}

// serveEventSource implements RFC 8620 section 7.3. It sends the current
// state on connect, then a StateChange for each change. Pings are not sent.
func (s *Server) serveEventSource(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	want := map[string]bool{}
	for _, t := range strings.Split(q.Get("types"), ",") {
		if t == "*" || t == "" {
			for _, pt := range pushTypes {
				want[pt] = true
			}
		}
		want[t] = true
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sub := &subscriber{wake: make(chan struct{}, 1), drop: make(chan struct{})}
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, sub)
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	sent := map[string]string{}
	for {
		s.mu.Lock()
		ts := jmap.TypeState{}
		for _, t := range pushTypes {
			if st := s.stateOf(t); want[t] && sent[t] != st {
				ts[t], sent[t] = st, st
			}
		}
		s.mu.Unlock()
		if len(ts) > 0 {
			b, _ := json.Marshal(&jmap.StateChange{Type: "StateChange", Changed: map[jmap.ID]jmap.TypeState{accountID: ts}})
			fmt.Fprintf(w, "event: state\ndata: %s\n\n", b)
			flusher.Flush()
			if q.Get("closeafter") == "state" {
				return
			}
		}
		select {
		case <-sub.wake:
		case <-sub.drop:
			return
		case <-r.Context().Done():
			return
		}
	}
}
