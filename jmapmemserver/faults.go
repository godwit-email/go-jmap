package jmapmemserver

import (
	"net/http/httptest"

	"git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
)

// MergeEmails replaces the given emails with one new email under a new id,
// as servers do when they merge duplicates: the old ids are destroyed and
// the new one created. The result has the first email's content and the
// union of all mailboxIds and keywords. It panics on an unknown id.
func (s *Server) MergeEmails(ids ...jmap.ID) jmap.ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	var merged email.Email
	merged.MailboxIDs, merged.Keywords = map[jmap.ID]bool{}, map[string]bool{}
	for i, id := range ids {
		e := s.email(id)
		if e == nil {
			panic("jmapmemserver: MergeEmails: no email " + string(id))
		}
		if i == 0 {
			mb, kw := merged.MailboxIDs, merged.Keywords
			merged = *e
			merged.MailboxIDs, merged.Keywords = mb, kw
		}
		for k := range e.MailboxIDs {
			merged.MailboxIDs[k] = true
		}
		for k := range e.Keywords {
			merged.Keywords[k] = true
		}
		s.destroyEmail(id)
	}
	merged.ID = s.newID("e")
	merged.ThreadID = merged.ID
	s.emails = append(s.emails, &merged)
	s.record("Email", merged.ID, created)
	return merged.ID
}

// RejectNextRequest makes the next HTTP request, whatever its path or
// credentials, fail with 401 Unauthorized.
func (s *Server) RejectNextRequest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectNext = true
}

// SeparateDownloadHost starts a second listener and points the session's
// downloadUrl at it, so downloads go to a different host:port than the API.
// It returns the new host's base URL. Close stops it too.
func (s *Server) SeparateDownloadHost() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dlSrv == nil {
		s.dlSrv = httptest.NewServer(s)
		s.sessionState++
	}
	return s.dlSrv.URL
}

// Redirect answers requests for path with 307 Temporary Redirect to
// location, before authentication. An empty location removes the redirect.
func (s *Server) Redirect(path, location string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if location == "" {
		delete(s.redirects, path)
	} else {
		s.redirects[path] = location
	}
}
