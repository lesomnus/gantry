package retention

import (
	"encoding/json"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	bolt "go.etcd.io/bbolt"
)

var (
	bktImg = []byte("img") // img/<engine>/<ref> -> json(Record)
	bktPin = []byte("pin") // pin/<engine>/<ref-or-pattern> -> json(PinEntry)
)

// PinEntry is one persisted pin: an exact reference, or — when Pattern — a
// doublestar pattern matched against the full ref, its name:tag short form,
// and the bare tag.
type PinEntry struct {
	Value   string    `json:"value"`
	Pattern bool      `json:"pattern,omitempty"`
	At      time.Time `json:"pinned_at,omitzero"` // zero for pins stored before entries carried a timestamp
}

// protects reports whether this pin protects the record. An exact pin only
// ever matches its own ref — it must not glob or short-name match.
func (e PinEntry) protects(r Record) bool {
	if !e.Pattern {
		return e.Value == r.Ref
	}
	return matchPin(e.Value, r)
}

// Index is the persisted last-used / pin store, shared by the usage watcher and
// the policy engine.
type Index struct{ db *bolt.DB }

func Open(path string) (*Index, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bktImg, bktPin} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Index{db: db}, nil
}

func (ix *Index) Close() error { return ix.db.Close() }

func sub(tx *bolt.Tx, top []byte, engine string) (*bolt.Bucket, error) {
	return tx.Bucket(top).CreateBucketIfNotExists([]byte(engine))
}

func (ix *Index) upsert(engine, ref string, fn func(*Record)) error {
	return ix.db.Update(func(tx *bolt.Tx) error {
		b, err := sub(tx, bktImg, engine)
		if err != nil {
			return err
		}
		var r Record
		if v := b.Get([]byte(ref)); v != nil {
			_ = json.Unmarshal(v, &r)
		}
		if r.Ref == "" {
			r.Ref = ref
			r.Repo, r.Tag, r.Digest = parseRef(ref)
		}
		fn(&r)
		enc, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return b.Put([]byte(ref), enc)
	})
}

// Touch records that ref was used at t (LastUsed = max(current, t)). Seed is the
// same merge, used to bootstrap from existing containers at startup.
func (ix *Index) Touch(engine, ref string, t time.Time) error {
	return ix.upsert(engine, ref, func(r *Record) {
		if r.FirstSeen.IsZero() {
			r.FirstSeen = t
		}
		if t.After(r.LastUsed) {
			r.LastUsed = t
		}
	})
}

func (ix *Index) Seed(engine, ref string, t time.Time) error { return ix.Touch(engine, ref, t) }

// Distributed records that gantry pushed ref to the engine at t. It does not set
// LastUsed (that stays a pure usage signal); effLastUsed falls back to it.
func (ix *Index) Distributed(engine, ref string, t time.Time) error {
	return ix.upsert(engine, ref, func(r *Record) {
		if r.FirstSeen.IsZero() {
			r.FirstSeen = t
		}
		r.LastDistributed = t
	})
}

// List returns every record for an engine, with Pinned set from the pin bucket
// (exact-ref pins and matching patterns alike).
func (ix *Index) List(engine string) ([]Record, error) {
	var out []Record
	err := ix.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktImg).Bucket([]byte(engine))
		if b == nil {
			return nil
		}
		var pins []PinEntry
		if pb := tx.Bucket(bktPin).Bucket([]byte(engine)); pb != nil {
			_ = pb.ForEach(func(k, v []byte) error {
				e := PinEntry{Value: string(k)}
				_ = json.Unmarshal(v, &e) // legacy raw markers decode as exact pins
				e.Value = string(k)
				pins = append(pins, e)
				return nil
			})
		}
		return b.ForEach(func(k, v []byte) error {
			var r Record
			if json.Unmarshal(v, &r) != nil {
				return nil
			}
			for _, pin := range pins {
				if pin.protects(r) {
					r.Pinned = true
					break
				}
			}
			out = append(out, r)
			return nil
		})
	})
	return out, err
}

func (ix *Index) Delete(engine, ref string) error {
	return ix.db.Update(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bktImg).Bucket([]byte(engine)); b != nil {
			return b.Delete([]byte(ref))
		}
		return nil
	})
}

func (ix *Index) Pin(engine, ref string, pattern bool) error {
	return ix.db.Update(func(tx *bolt.Tx) error {
		b, err := sub(tx, bktPin, engine)
		if err != nil {
			return err
		}
		enc, err := json.Marshal(PinEntry{Value: ref, Pattern: pattern, At: time.Now().UTC()})
		if err != nil {
			return err
		}
		return b.Put([]byte(ref), enc)
	})
}

func (ix *Index) Unpin(engine, ref string) error {
	return ix.db.Update(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bktPin).Bucket([]byte(engine)); b != nil {
			return b.Delete([]byte(ref))
		}
		return nil
	})
}

func (ix *Index) Pins(engine string) ([]PinEntry, error) {
	var out []PinEntry
	err := ix.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktPin).Bucket([]byte(engine))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			e := PinEntry{Value: string(k)}
			// Pins stored before entries carried metadata hold a raw marker byte;
			// the key is still the pin value.
			_ = json.Unmarshal(v, &e)
			e.Value = string(k)
			out = append(out, e)
			return nil
		})
	})
	return out, err
}

// Counts tallies an engine's records and pins with a key-only walk — no
// decoding or pin matching (polled by the metrics observer on every collection).
func (ix *Index) Counts(engine string) (records, pins int, err error) {
	err = ix.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bktImg).Bucket([]byte(engine)); b != nil {
			records = b.Stats().KeyN
		}
		if b := tx.Bucket(bktPin).Bucket([]byte(engine)); b != nil {
			pins = b.Stats().KeyN
		}
		return nil
	})
	return records, pins, err
}

// parseRef splits a reference into its host-qualified repo (the keep-N grouping
// key), tag, and digest.
func parseRef(ref string) (repo, tag, digest string) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return ref, "", ""
	}
	repo = r.Context().Name()
	if d, ok := r.(name.Digest); ok {
		digest = d.DigestStr()
	} else {
		tag = r.Identifier()
	}
	return repo, tag, digest
}
