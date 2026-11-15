// Package wal persists Raft state in plain files written by our own code:
// an append-only log of CRC-checked records (hard state; entries) and a
// snapshot file replaced atomically. Every Save ends with fsync (on macOS
// Go's File.Sync issues F_FULLFSYNC), so nothing is acknowledged before it
// is durable.
//
// Record: u32 length | u32 CRC-32C of (type|payload) | u8 type | payload.
// On replay a later entry at index i replaces every entry from i on, which
// is how a conflicting suffix is overwritten. A torn tail (crash mid-write)
// fails its length or CRC check and is truncated away.
package wal

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/raft"
)

const (
	recHardState byte = 1
	recEntry     byte = 2
	maxRecord         = 64 << 20

	walName  = "wal.log"
	snapName = "snap.bin"
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Storage is one node's durable state.
type Storage struct {
	dir     string
	f       *os.File
	hs      raft.HardState
	snap    *raft.Snapshot
	entries []raft.Entry // since snap
}

// Open loads (or creates) the state in dir.
func Open(dir string) (*Storage, raft.Restored, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, raft.Restored{}, err
	}
	s := &Storage{dir: dir}
	if err := s.loadSnapshot(); err != nil {
		return nil, raft.Restored{}, err
	}
	good, err := s.replay()
	if err != nil {
		return nil, raft.Restored{}, err
	}
	f, err := os.OpenFile(filepath.Join(dir, walName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, raft.Restored{}, err
	}
	// Drop a torn tail so new records append after the last good one.
	if err := f.Truncate(good); err != nil {
		f.Close()
		return nil, raft.Restored{}, err
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, raft.Restored{}, err
	}
	s.f = f
	return s, s.restored(), nil
}

func (s *Storage) restored() raft.Restored {
	r := raft.Restored{HardState: s.hs, Entries: append([]raft.Entry(nil), s.entries...)}
	if s.snap != nil {
		cp := *s.snap
		r.Snapshot = &cp
	}
	return r
}

func (s *Storage) firstIndex() uint64 {
	if s.snap != nil {
		return s.snap.Index + 1
	}
	return 1
}

// put applies an entry with "later index i truncates from i" semantics.
func (s *Storage) put(e raft.Entry) {
	first := s.firstIndex()
	if e.Index < first {
		return
	}
	pos := int(e.Index - first)
	if pos > len(s.entries) {
		pos = len(s.entries) // a gap can't happen with correct callers; keep contiguous
	}
	s.entries = append(s.entries[:pos], e)
}

// replay reads the WAL and returns the offset after the last good record.
func (s *Storage) replay() (int64, error) {
	f, err := os.Open(filepath.Join(s.dir, walName))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var off int64
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return off, nil // clean end or torn header
		}
		n := binary.BigEndian.Uint32(hdr[0:4])
		sum := binary.BigEndian.Uint32(hdr[4:8])
		if n == 0 || n > maxRecord {
			return off, nil
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return off, nil // torn body
		}
		if crc32.Checksum(body, crcTable) != sum {
			return off, nil // torn or corrupted: stop at the last good record
		}
		switch body[0] {
		case recHardState:
			if len(body) != 25 {
				return off, fmt.Errorf("wal: bad hard state record at %d", off)
			}
			s.hs = raft.HardState{Term: binary.BigEndian.Uint64(body[1:]), Vote: binary.BigEndian.Uint64(body[9:]), Commit: binary.BigEndian.Uint64(body[17:])}
		case recEntry:
			if len(body) < 17 {
				return off, fmt.Errorf("wal: bad entry record at %d", off)
			}
			e := raft.Entry{Term: binary.BigEndian.Uint64(body[1:]), Index: binary.BigEndian.Uint64(body[9:])}
			if len(body) > 17 {
				e.Data = append([]byte(nil), body[17:]...)
			} // empty payload: the leader's no-op (nil Data)
			s.put(e)
		default:
			return off, fmt.Errorf("wal: unknown record type %d at %d", body[0], off)
		}
		off += int64(8 + n)
	}
}

func appendRecord(buf []byte, typ byte, payload []byte) []byte {
	body := make([]byte, 0, 1+len(payload))
	body = append(body, typ)
	body = append(body, payload...)
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(len(body)))
	binary.BigEndian.PutUint32(hdr[4:8], crc32.Checksum(body, crcTable))
	return append(append(buf, hdr[:]...), body...)
}

func hsPayload(hs raft.HardState) []byte {
	var b [24]byte
	binary.BigEndian.PutUint64(b[0:], hs.Term)
	binary.BigEndian.PutUint64(b[8:], hs.Vote)
	binary.BigEndian.PutUint64(b[16:], hs.Commit)
	return b[:]
}

func entryPayload(e raft.Entry) []byte {
	b := make([]byte, 16, 16+len(e.Data))
	binary.BigEndian.PutUint64(b[0:], e.Term)
	binary.BigEndian.PutUint64(b[8:], e.Index)
	return append(b, e.Data...)
}

// Save makes a Ready durable: snapshot first (atomic file replace), then
// hard state and entries appended to the WAL, then fsync.
func (s *Storage) Save(hs *raft.HardState, entries []raft.Entry, snap *raft.Snapshot) error {
	if snap != nil {
		if err := s.SaveSnapshot(*snap); err != nil {
			return err
		}
	}
	if hs == nil && len(entries) == 0 {
		return nil
	}
	var buf []byte
	for _, e := range entries {
		buf = appendRecord(buf, recEntry, entryPayload(e))
		s.put(e)
	}
	if hs != nil {
		buf = appendRecord(buf, recHardState, hsPayload(*hs))
		s.hs = *hs
	}
	if _, err := s.f.Write(buf); err != nil {
		return err
	}
	return s.f.Sync()
}

// SaveSnapshot writes a snapshot atomically and rewrites the WAL without
// the entries it covers.
func (s *Storage) SaveSnapshot(snap raft.Snapshot) error {
	if s.snap != nil && snap.Index <= s.snap.Index {
		return nil
	}
	var b []byte
	b = binary.BigEndian.AppendUint64(b, snap.Index)
	b = binary.BigEndian.AppendUint64(b, snap.Term)
	b = append(b, snap.Data...)
	b = binary.BigEndian.AppendUint32(b, crc32.Checksum(b, crcTable))
	if err := writeAtomic(s.dir, snapName, b); err != nil {
		return err
	}
	cp := snap
	s.snap = &cp
	kept := s.entries[:0:0]
	for _, e := range s.entries {
		if e.Index > snap.Index {
			kept = append(kept, e)
		}
	}
	s.entries = kept
	return s.rewriteWAL()
}

func (s *Storage) rewriteWAL() error {
	var buf []byte
	for _, e := range s.entries {
		buf = appendRecord(buf, recEntry, entryPayload(e))
	}
	buf = appendRecord(buf, recHardState, hsPayload(s.hs))
	if err := writeAtomic(s.dir, walName, buf); err != nil {
		return err
	}
	if s.f != nil {
		s.f.Close()
	}
	f, err := os.OpenFile(filepath.Join(s.dir, walName), os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return err
	}
	s.f = f
	return nil
}

func (s *Storage) loadSnapshot() error {
	b, err := os.ReadFile(filepath.Join(s.dir, snapName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(b) < 20 || crc32.Checksum(b[:len(b)-4], crcTable) != binary.BigEndian.Uint32(b[len(b)-4:]) {
		return errors.New("wal: snapshot file is corrupt")
	}
	s.snap = &raft.Snapshot{Index: binary.BigEndian.Uint64(b[0:]), Term: binary.BigEndian.Uint64(b[8:]),
		Data: append([]byte(nil), b[16:len(b)-4]...)}
	return nil
}

// writeAtomic writes name via a temp file, fsync, rename and directory fsync.
func writeAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Close closes the WAL file.
func (s *Storage) Close() error { return s.f.Close() }
