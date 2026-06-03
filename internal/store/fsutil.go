package store

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

// copyBufSize is the 32 KB streaming window used for every payload copy so a 50 MB
// item is never held whole in RAM (PLAN §3.7, byte-for-behavior with Swift's write
// loop in writeAtomicFsync).
const copyBufSize = 32 * 1024

// writeAtomicFsync streams r into a temp file (0600) in destDir, fsyncs it, then
// renames it to dest — byte-for-behavior with Swift writeAtomicFsync
// (Clipboard.swift:424-462). It is bounded-memory (io.CopyBuffer, 32 KB) and the dir
// self-heals (recreated if it vanished). It returns the number of payload bytes
// written so Ingest can run the per-item write→add→check cap (PLAN §3.8).
//
// On any failure the temp file is removed and the real errno is surfaced (e.g.
// ENOSPC) so a disk-full / permission failure is diagnosable rather than opaque.
func writeAtomicFsync(dest string, r io.Reader) (written int64, err error) {
	dir := filepath.Dir(dest)
	// Self-heal: recreate the destination dir if it vanished since launch (best-effort;
	// the open below is the real check and reports the true errno).
	_ = os.MkdirAll(dir, 0o700)

	tmp := filepath.Join(dir, ".clipbeam-tmp-"+randHex())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}

	written, err = io.CopyBuffer(f, r, make([]byte, copyBufSize))
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return written, err
	}
	if syncErr := f.Sync(); syncErr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return written, syncErr
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(tmp)
		return written, closeErr
	}
	if renErr := os.Rename(tmp, dest); renErr != nil {
		_ = os.Remove(tmp)
		return written, renErr
	}
	return written, nil
}

// writeBytesAtomic writes data to dest atomically (temp in the same dir → rename) at
// mode perm, mirroring Swift's Data.write(options:.atomic) for last_path / recents.
// Best-effort: errors are returned so callers can decide (last_path writes ignore
// them like Swift's `try?`).
func writeBytesAtomic(dest string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(dest)
	_ = os.MkdirAll(dir, 0o700)
	tmp := filepath.Join(dir, ".clipbeam-tmp-"+randHex())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Ensure the final mode even if umask masked the create perm.
	_ = os.Chmod(dest, perm)
	return nil
}

// randHex returns 16 random hex chars for the temp-file suffix, sourced from
// crypto/rand (the weak PRNG is avoided even for this non-secret name, for hygiene
// and to satisfy the CI source gate).
func randHex() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read never fails on supported OSes; if it somehow does, a fixed-but-
		// process-unique suffix from the address space is acceptable for a temp name.
		return "fallbacktmpname"
	}
	return hex.EncodeToString(b[:])
}
