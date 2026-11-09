package jmapmemserver

import (
	"strconv"

	"git.sr.ht/~rockorager/go-jmap"
)

type changeKind int

const (
	created changeKind = iota
	updated
	destroyed
)

type change struct {
	seq  int
	typ  string // "Mailbox" or "Email"
	id   jmap.ID
	kind changeKind
}

// record logs a change with the lock held. Every change gets its own modseq;
// a type's state is the modseq of its latest change.
func (s *Server) record(typ string, id jmap.ID, kind changeKind) {
	s.modseq++
	s.log = append(s.log, change{s.modseq, typ, id, kind})
	s.typeState[typ] = s.modseq
	s.wakeSubscribers()
}

func (s *Server) stateOf(typ string) string { return strconv.Itoa(s.typeState[typ]) }

// ExpireState makes every state handed out so far too old for /changes,
// which then fail with cannotCalculateChanges until the client refetches.
func (s *Server) ExpireState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modseq++
	s.minSeq = s.modseq
	s.log = nil
	for _, typ := range []string{"Mailbox", "Email"} {
		s.typeState[typ] = s.modseq
	}
}

type changeSet struct {
	oldState, newState          string
	hasMore                     bool
	created, updated, destroyed []jmap.ID
}

func (s *Server) changes(typ, since string, max uint64) (*changeSet, *jmap.MethodError) {
	n, err := strconv.Atoi(since)
	if err != nil || n < s.minSeq || n > s.modseq {
		return nil, methodErr("cannotCalculateChanges")
	}
	type seen struct{ created, updated, destroyed bool }
	var order []jmap.ID
	ids := map[jmap.ID]*seen{}
	cs := &changeSet{oldState: since, newState: s.stateOf(typ)}
	last := n
	for _, c := range s.log {
		if c.seq <= n || c.typ != typ {
			continue
		}
		st := ids[c.id]
		if st == nil {
			if max > 0 && uint64(len(order)) == max {
				cs.hasMore = true
				cs.newState = strconv.Itoa(last)
				break
			}
			st = &seen{}
			ids[c.id] = st
			order = append(order, c.id)
		}
		switch c.kind {
		case created:
			st.created = true
		case updated:
			st.updated = true
		case destroyed:
			st.destroyed = true
		}
		last = c.seq
	}
	for _, id := range order {
		switch st := ids[id]; {
		case st.destroyed:
			cs.destroyed = append(cs.destroyed, id)
		case st.created:
			cs.created = append(cs.created, id)
		default:
			cs.updated = append(cs.updated, id)
		}
	}
	return cs, nil
}
