package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	yaml "gopkg.in/yaml.v3"
)

type HashStore interface {
	// Add a hash to the HashStore.
	Add(name, hash string) error

	// Get a hash from the HashStore.
	Get(name string) (string, error)

	// Persist the hashes.
	Save() error
}

const (
	HashStrategyReadWrite = "readwrite"
	HashStrategyRead      = "read"
)

// An implementation of the HashStore that stores all hashes inside a single
// JSON file.
type JSONHashStore struct {
	path     string
	hashes   map[string]string
	strategy string
	mu       sync.RWMutex
}

func NewJSONHashStore(path, strategy string) (*JSONHashStore, error) {
	hashes := make(map[string]string)
	content, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		if err := json.Unmarshal(content, &hashes); err != nil {
			// If the file is invalid JSON, instead of erroring out,
			// just start from scratch so a new valid file is
			// generated.
			log.Printf("Unable to parse hashes from %s: %v\n", path, err)
		}
	}

	hashes["//"] = "AUTO GENERATED. DO NOT EDIT."

	return &JSONHashStore{
		path:     path,
		hashes:   hashes,
		strategy: strategy,
	}, nil
}

func (s *JSONHashStore) Add(name, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hashes[name] = hash
	return nil
}

func (s *JSONHashStore) Get(name string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hashes[name], nil
}

func (s *JSONHashStore) Save() error {
	if s.strategy == HashStrategyRead {
		// Read-only mode, so don't write.
		return nil
	}

	s.mu.RLock()
	b, err := json.MarshalIndent(s.hashes, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}

	return os.WriteFile(s.path, b, 0644)
}

type ChartHash struct {
	Hash string `yaml:"hash"`
}

// An implementation of HashStore that stores hashes in a "hash.sum" file.
type SumFileStore struct {
	path     string
	strategy string
}

func NewSumFileStore(path, strategy string) *SumFileStore {
	return &SumFileStore{
		path:     path,
		strategy: strategy,
	}
}

func (s *SumFileStore) Add(name, hash string) error {
	ch := ChartHash{
		Hash: hash,
	}
	data, err := yaml.Marshal(&ch)
	if err != nil {
		return err
	}

	if s.strategy == HashStrategyRead {
		// Read-only mode, don't write
		return nil
	}

	return os.WriteFile(s.filepath(name), data, 0664)
}

func (s *SumFileStore) Get(name string) (string, error) {
	filepath := s.filepath(name)

	yfile, err := os.ReadFile(filepath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// This is fine to do since there are cases where there won't be a hash. e.g. root
			return "", nil
		}
		return "", fmt.Errorf("error reading file hash from %s error: %w", filepath, err)
	}
	ch := ChartHash{}
	err2 := yaml.Unmarshal(yfile, &ch)
	if err2 != nil {
		return "", fmt.Errorf("error unmarshaling hash %s error: %w", filepath, err2)
	}
	return ch.Hash, nil
}

func (s *SumFileStore) Save() error {
	// Already written in Add
	return nil
}

func (s *SumFileStore) filepath(name string) string {
	return filepath.Join(s.path, name, "hash.sum")
}
