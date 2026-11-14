package main

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

// GPULock coordinates single-concurrency GPU execution across goroutines and processes.
type GPULock struct {
	path string
	mu   sync.Mutex
	file *os.File
}

func NewGPULock(lockPath string) *GPULock {
	return &GPULock{
		path: lockPath,
	}
}

// TryLock attempts to acquire the GPU lock without blocking. Returns true if acquired.
func (l *GPULock) TryLock() bool {
	if !l.mu.TryLock() {
		return false
	}

	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		l.mu.Unlock()
		return false
	}

	// Non-blocking flock
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		l.mu.Unlock()
		return false
	}

	l.file = f
	return true
}

// Lock blocks until the GPU lock is acquired.
func (l *GPULock) Lock() error {
	l.mu.Lock()

	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		l.mu.Unlock()
		return fmt.Errorf("failed to open lockfile %s: %w", l.path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		l.mu.Unlock()
		return fmt.Errorf("failed to flock GPU lockfile: %w", err)
	}

	l.file = f
	return nil
}

// Unlock releases the GPU lock.
func (l *GPULock) Unlock() {
	if l.file != nil {
		syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		l.file.Close()
		l.file = nil
		os.Remove(l.path)
	}
	l.mu.Unlock()
}
