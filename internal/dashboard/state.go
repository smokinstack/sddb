package dashboard

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/jester/sddb/internal/types"
)

// AgentRecord tracks a known agent and its most recent stats.
type AgentRecord struct {
	ID         string
	Addr       string // host:port
	Label      string // user-defined friendly name (optional)
	LastSeen   time.Time
	Online     bool
	LastStats  types.StatsResponse
	ErrMessage string
}

// State holds all known agents; safe for concurrent access.
type State struct {
	mu      sync.RWMutex
	agents  map[string]*AgentRecord // keyed by agent ID
	persist string                  // path to persist agents list
}

func NewState(persistPath string) *State {
	s := &State{
		agents:  make(map[string]*AgentRecord),
		persist: persistPath,
	}
	s.load()
	return s
}

// AddAgent registers or updates a known agent by address.
// Returns the existing record if already known.
func (s *State) AddAgent(addr, label string) *AgentRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if we already have an agent at this address
	for _, r := range s.agents {
		if r.Addr == addr {
			if label != "" {
				r.Label = label
			}
			s.save()
			return r
		}
	}

	// New agent — use addr as temporary ID until first successful poll
	r := &AgentRecord{
		ID:    addr,
		Addr:  addr,
		Label: label,
	}
	s.agents[addr] = r
	s.save()
	return r
}

// UpdateFromPoll updates an agent's state after a successful poll.
func (s *State) UpdateFromPoll(addr string, resp types.StatsResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.findByAddr(addr)
	if !ok {
		return
	}

	oldID := r.ID
	r.ID = resp.Agent.ID
	r.LastSeen = time.Now()
	r.Online = true
	r.LastStats = resp
	r.ErrMessage = ""

	// Re-key the map if the ID changed from the address placeholder
	if oldID != r.ID {
		delete(s.agents, oldID)
		s.agents[r.ID] = r
	}
}

// MarkOffline marks an agent as offline with an error message.
func (s *State) MarkOffline(addr, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.findByAddr(addr)
	if !ok {
		return
	}
	r.Online = false
	r.ErrMessage = errMsg
}

// RemoveAgent removes an agent by its ID.
func (s *State) RemoveAgent(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.agents[id]; !ok {
		return false
	}
	delete(s.agents, id)
	s.save()
	return true
}

// All returns a snapshot of all agent records sorted stably by hostname then address.
func (s *State) All() []*AgentRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	records := make([]*AgentRecord, 0, len(s.agents))
	for _, r := range s.agents {
		cp := *r
		records = append(records, &cp)
	}
	sort.Slice(records, func(i, j int) bool {
		hi := records[i].LastStats.Agent.Hostname
		hj := records[j].LastStats.Agent.Hostname
		if hi == "" {
			hi = records[i].Addr
		}
		if hj == "" {
			hj = records[j].Addr
		}
		if hi != hj {
			return hi < hj
		}
		return records[i].Addr < records[j].Addr
	})
	return records
}

// Addresses returns all known agent addresses (for the poller).
func (s *State) Addresses() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	addrs := make([]string, 0, len(s.agents))
	for _, r := range s.agents {
		addrs = append(addrs, r.Addr)
	}
	return addrs
}

func (s *State) findByAddr(addr string) (*AgentRecord, bool) {
	for _, r := range s.agents {
		if r.Addr == addr {
			return r, true
		}
	}
	return nil, false
}

// save persists the agent list (addresses + labels) to disk.
func (s *State) save() {
	if s.persist == "" {
		return
	}
	type entry struct {
		Addr  string `json:"addr"`
		Label string `json:"label"`
	}
	entries := make([]entry, 0, len(s.agents))
	for _, r := range s.agents {
		entries = append(entries, entry{Addr: r.Addr, Label: r.Label})
	}
	data, _ := json.MarshalIndent(entries, "", "  ")
	os.WriteFile(s.persist, data, 0600)
}

// load restores persisted agents from disk.
func (s *State) load() {
	if s.persist == "" {
		return
	}
	data, err := os.ReadFile(s.persist)
	if err != nil {
		return
	}
	type entry struct {
		Addr  string `json:"addr"`
		Label string `json:"label"`
	}
	var entries []entry
	if json.Unmarshal(data, &entries) != nil {
		return
	}
	for _, e := range entries {
		if e.Addr == "" {
			continue
		}
		s.agents[e.Addr] = &AgentRecord{
			ID:    e.Addr,
			Addr:  e.Addr,
			Label: e.Label,
		}
	}
}
