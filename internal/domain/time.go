package domain

import (
	"fmt"
	"time"
)

// MongoTime marshals to the same RFC3339-with-milliseconds format Mongoose used
// (e.g. "2026-10-05T13:34:11.623Z"). Go's default time.Time marshalling drops
// trailing zeros, so a timestamp with .000 would render "…T13:34:11Z" and break
// Byte-for-byte comparison with the Node responses.
type MongoTime struct {
	time.Time
}

func NewMongoTime(t time.Time) MongoTime { return MongoTime{Time: t} }

func (m MongoTime) MarshalJSON() ([]byte, error) {
	if m.Time.IsZero() {
		return []byte("null"), nil
	}
	s := m.Time.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	return []byte(`"` + s + `"`), nil
}

func (m *MongoTime) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" || s == `""` {
		return nil
	}
	t, err := time.Parse(`"`+time.RFC3339Nano+`"`, s)
	if err != nil {
		return err
	}
	m.Time = t
	return nil
}

// Scan implements sql.Scanner so timestamps read from Postgres populate MongoTime
// directly. Without it, scanning a timestamptz into this type fails at runtime.
func (m *MongoTime) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		return nil
	case time.Time:
		m.Time = v
		return nil
	case string:
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return err
		}
		m.Time = t
		return nil
	default:
		return fmt.Errorf("cannot scan %T into MongoTime", value)
	}
}

// Value implements driver.Valuer for symmetry with Scan.
func (m MongoTime) Value() (any, error) {
	if m.Time.IsZero() {
		return nil, nil
	}
	return m.Time, nil
}
