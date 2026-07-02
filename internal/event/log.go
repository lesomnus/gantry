// Package event is a small append-only audit log persisted in bbolt: the
// operationally significant things gantry did — jobs admitted and finished, GC
// applied, manual pulls/removes, pins — that would otherwise vanish when the
// in-memory job store is lost on restart. It is bounded (a ring keeps the last
// N entries) and independent of the retention index so it works even when GC is
// disabled.
package event

import (
	"encoding/binary"
	"encoding/json"
	"time"

	bolt "go.etcd.io/bbolt"
)

var bktEvt = []byte("evt") // evt/<seq> -> json(Event)

// Type classifies an event.
type Type string

const (
	JobAdmitted Type = "job_admitted"
	JobDone     Type = "job_done"
	GCApplied   Type = "gc_applied"
	ImagePulled Type = "image_pulled"
	ImageRemove Type = "image_removed"
	Pinned      Type = "pinned"
	Unpinned    Type = "unpinned"
)

// Event is one audit record. Seq is a monotonic id; the rest is best-effort
// context, present per Type.
type Event struct {
	Seq    uint64          `json:"seq"`
	At     time.Time       `json:"at"`
	Type   Type            `json:"type"`
	Store  string          `json:"store,omitempty"`
	Ref    string          `json:"ref,omitempty"`
	State  string          `json:"state,omitempty"`
	Digest string          `json:"digest,omitempty"`
	Error  string          `json:"error,omitempty"`
	Detail json.RawMessage `json:"detail,omitempty" swaggertype:"object"`
}

// Filter narrows a Log query.
type Filter struct {
	Type  Type
	Store string
	Ref   string
	State string
	Since time.Time
	Limit int // newest-first cap; 0 = a default cap
}

// Log is the bbolt-backed ring.
type Log struct {
	db  *bolt.DB
	cap int // max retained entries; oldest are evicted past this
	now func() time.Time
}

// Open creates or opens the log at path, keeping at most capEntries records.
func Open(path string, capEntries int) (*Log, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bktEvt)
		return err
	}); err != nil {
		db.Close()
		return nil, err
	}
	if capEntries <= 0 {
		capEntries = 10000
	}
	return &Log{db: db, cap: capEntries, now: time.Now}, nil
}

func (l *Log) Close() error { return l.db.Close() }

// Append records e (Seq/At are assigned here) and evicts the oldest entries
// past the cap. A logging failure must never break the operation it records, so
// callers ignore the error beyond a debug log.
func (l *Log) Append(e Event) error {
	return l.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktEvt)
		seq, _ := b.NextSequence()
		e.Seq = seq
		if e.At.IsZero() {
			e.At = l.now().UTC()
		}
		enc, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if err := b.Put(itob(seq), enc); err != nil {
			return err
		}
		// Ring eviction: keys are monotonic sequences, so anything at or before
		// (newest - cap) is beyond the window. (Bucket.Stats().KeyN is not
		// reliable mid-transaction, so bound by sequence instead.)
		//
		// Collect then delete — deleting under a forward cursor is unsafe in
		// bbolt (Delete shifts the node's inodes and the following Next steps
		// over the entry that slid down), which would skip every other key when
		// a single Append must evict a backlog (e.g. after lowering the cap).
		if seq > uint64(l.cap) {
			cutoff := itob(seq - uint64(l.cap))
			var stale [][]byte
			c := b.Cursor()
			for k, _ := c.First(); k != nil && string(k) <= string(cutoff); k, _ = c.Next() {
				stale = append(stale, append([]byte(nil), k...)) // k is only valid until Next
			}
			for _, k := range stale {
				if err := b.Delete(k); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// List returns matching events newest-first, capped by Filter.Limit (default
// 100, hard max 1000).
func (l *Log) List(f Filter) ([]Event, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	var out []Event
	err := l.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bktEvt).Cursor()
		for k, v := c.Last(); k != nil && len(out) < limit; k, v = c.Prev() {
			var e Event
			if json.Unmarshal(v, &e) != nil {
				continue
			}
			if f.Type != "" && e.Type != f.Type {
				continue
			}
			if f.Store != "" && e.Store != f.Store {
				continue
			}
			if f.Ref != "" && e.Ref != f.Ref {
				continue
			}
			if f.State != "" && e.State != f.State {
				continue
			}
			if !f.Since.IsZero() && e.At.Before(f.Since) {
				continue
			}
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

func itob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// detail helpers keep event bodies compact without a struct per call site.
func kv(k, v string) json.RawMessage {
	if v == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]string{k: v})
	return b
}

func kvi(id string, bytes int64) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"job": id, "bytes": bytes})
	return b
}

func gcDetail(deleted, untagged, errs int) json.RawMessage {
	b, _ := json.Marshal(map[string]int{"deleted": deleted, "untagged": untagged, "errors": errs})
	return b
}
