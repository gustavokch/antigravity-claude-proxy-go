package mitm

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// Session is one observed cloud session. ID is a 12-character hash: the raw
// session id is never stored.
type Session struct {
	ID               string    `json:"id"`
	EnvironmentKind  string    `json:"environmentKind,omitempty"`
	Model            string    `json:"model,omitempty"`
	SessionStatus    string    `json:"sessionStatus,omitempty"`
	StatusBucket     string    `json:"statusBucket,omitempty"`
	ConnectionStatus string    `json:"connectionStatus,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	LastSeenAt       time.Time `json:"lastSeenAt"`
	Requests         int       `json:"requests"`
	LastRoute        string    `json:"lastRoute,omitempty"`
}

// Registry is a capped, TTL-bound, in-memory store of observed sessions.
type Registry struct {
	mu   sync.Mutex
	max  int
	ttl  time.Duration
	now  func() time.Time
	byID map[string]*Session
}

// NewRegistry returns a registry holding at most max sessions for ttl each.
func NewRegistry(max int, ttl time.Duration, now func() time.Time) *Registry {
	if max <= 0 {
		max = 1000
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if now == nil {
		now = time.Now
	}
	return &Registry{max: max, ttl: ttl, now: now, byID: map[string]*Session{}}
}

// HashID reduces a raw session id to its display id.
func HashID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:12]
}

// Observe merges one observation. Observations without a session id are
// ignored: the registry only tracks sessions.
func (r *Registry) Observe(o Observation) {
	if o.RawID == "" {
		return
	}
	id := HashID(o.RawID)
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)
	s, ok := r.byID[id]
	if !ok {
		s = &Session{ID: id, CreatedAt: now, LastSeenAt: now}
		r.byID[id] = s
		r.evictLocked()
	}
	s.LastSeenAt = now
	s.Requests++
	s.LastRoute = o.Route
	if v := o.Fields["environmentKind"]; v != "" {
		s.EnvironmentKind = v
	}
	if v := o.Fields["model"]; v != "" {
		s.Model = v
	}
	if v := o.Fields["sessionStatus"]; v != "" {
		s.SessionStatus = v
	}
	if v := o.Fields["statusBucket"]; v != "" {
		s.StatusBucket = v
	}
	if v := o.Fields["connectionStatus"]; v != "" {
		s.ConnectionStatus = v
	}
	if v := o.Fields["createdAt"]; v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			s.CreatedAt = t
		}
	}
}

func (r *Registry) expireLocked(now time.Time) {
	for id, s := range r.byID {
		if now.Sub(s.LastSeenAt) > r.ttl {
			delete(r.byID, id)
		}
	}
}

func (r *Registry) evictLocked() {
	for len(r.byID) > r.max {
		var oldestID string
		var oldest time.Time
		for id, s := range r.byID {
			if oldestID == "" || s.LastSeenAt.Before(oldest) {
				oldestID, oldest = id, s.LastSeenAt
			}
		}
		delete(r.byID, oldestID)
	}
}

// List returns all live sessions, most recently seen first.
func (r *Registry) List() []Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(r.now())
	out := make([]Session, 0, len(r.byID))
	for _, s := range r.byID {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeenAt.After(out[j].LastSeenAt) })
	return out
}

// Get returns one session by its display id.
func (r *Registry) Get(id string) (Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(r.now())
	s, ok := r.byID[id]
	if !ok {
		return Session{}, false
	}
	return *s, true
}
