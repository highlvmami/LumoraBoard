package export

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/highlvmami/lumoraboard/backend/internal/board"
	"github.com/highlvmami/lumoraboard/backend/internal/store"
)

const (
	backupFormat  = "lumoraboard.board"
	backupVersion = 1
	// MaxBackupBytes caps an uploaded backup.
	MaxBackupBytes = 16 << 20
)

// Backup is the JSON export, and what import reads back.
type Backup struct {
	Format     string         `json:"format"`
	Version    int            `json:"version"`
	Board      string         `json:"board"`
	ExportedAt time.Time      `json:"exportedAt"`
	Objects    []board.Object `json:"objects"`
}

// ErrBadBackup wraps everything wrong with an uploaded backup.
var ErrBadBackup = errors.New("not a valid LumoraBoard backup")

// ParseBackup checks a backup and turns it into a snapshot for a new
// board. Every object goes through the same validation as a live "add"
// op, so an import can never hold something the room would refuse.
func ParseBackup(data []byte) (store.Snapshot, error) {
	var b Backup
	if err := json.Unmarshal(data, &b); err != nil {
		return store.Snapshot{}, fmt.Errorf("%w: %w", ErrBadBackup, err)
	}
	if b.Format != backupFormat {
		return store.Snapshot{}, fmt.Errorf("%w: format %q", ErrBadBackup, b.Format)
	}
	if b.Version != backupVersion {
		return store.Snapshot{}, fmt.Errorf("%w: version %d is not supported", ErrBadBackup, b.Version)
	}
	if len(b.Objects) > board.MaxObjects {
		return store.Snapshot{}, fmt.Errorf("%w: more than %d objects", ErrBadBackup, board.MaxObjects)
	}
	st := board.NewState()
	for i, o := range b.Objects {
		o.Version, o.CreatedBy = 0, ""
		raw, err := json.Marshal(board.Op{Kind: board.OpAdd, ID: o.ID, Object: &o})
		if err != nil {
			return store.Snapshot{}, fmt.Errorf("%w: %w", ErrBadBackup, err)
		}
		op, err := board.DecodeOp(raw)
		if err == nil {
			err = st.Apply(op, uint64(i+1), "import")
		}
		if err != nil {
			return store.Snapshot{}, fmt.Errorf("%w: object %d: %w", ErrBadBackup, i, err)
		}
	}
	objs := st.Snapshot()
	return store.Snapshot{Seq: uint64(len(objs)), Objects: slices.Clip(objs)}, nil
}
