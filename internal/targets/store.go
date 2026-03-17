package targets

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sky22333/qqbot/message"
)

type KnownTarget struct {
	TargetType    message.TargetType `json:"target_type"`
	TargetID      string             `json:"target_id"`
	FirstSeenAt   time.Time          `json:"first_seen_at"`
	LastSeenAt    time.Time          `json:"last_seen_at"`
	LastMessageID string             `json:"last_message_id,omitempty"`
	LastContent   string             `json:"last_content,omitempty"`
	SeenCount     int64              `json:"seen_count"`
}

type Store struct {
	filePath      string
	maxRecords    int
	flushInterval time.Duration

	mu      sync.RWMutex
	records map[string]KnownTarget
	latest  map[message.TargetType]KnownTarget
	latestX KnownTarget
	hasLast bool
	dirty   bool
	version uint64

	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	flushSig chan struct{}
}

func NewStore(filePath string, maxRecords int, flushInterval time.Duration) (*Store, error) {
	if strings.TrimSpace(filePath) == "" {
		return nil, errors.New("targets file path 不能为空")
	}
	if maxRecords <= 0 {
		maxRecords = 10000
	}
	if flushInterval <= 0 {
		flushInterval = 2 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Store{
		filePath:      filePath,
		maxRecords:    maxRecords,
		flushInterval: flushInterval,
		records:       make(map[string]KnownTarget),
		latest:        make(map[message.TargetType]KnownTarget),
		ctx:           ctx,
		cancel:        cancel,
		flushSig:      make(chan struct{}, 1),
	}
	if err := s.load(); err != nil {
		cancel()
		return nil, err
	}
	s.startFlusher()
	return s, nil
}

func (s *Store) Upsert(targetType message.TargetType, targetID, msgID, content string) error {
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return nil
	}
	key := string(targetType) + ":" + targetID
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.records[key]
	if !ok {
		record = KnownTarget{
			TargetType:  targetType,
			TargetID:    targetID,
			FirstSeenAt: now,
		}
	}
	record.LastSeenAt = now
	record.LastMessageID = strings.TrimSpace(msgID)
	record.LastContent = strings.TrimSpace(content)
	record.SeenCount++
	s.records[key] = record
	s.latest[record.TargetType] = record
	s.latestX = record
	s.hasLast = true
	s.compactIfNeededLocked()
	s.dirty = true
	s.version++
	s.signalFlushLocked()
	return nil
}

func (s *Store) List(limit int, targetType string) []KnownTarget {
	s.mu.RLock()
	defer s.mu.RUnlock()

	targetType = strings.TrimSpace(strings.ToLower(targetType))
	items := make([]KnownTarget, 0, len(s.records))
	for _, r := range s.records {
		if targetType != "" && string(r.TargetType) != targetType {
			continue
		}
		items = append(items, r)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].LastSeenAt.After(items[j].LastSeenAt)
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (s *Store) Latest(targetType message.TargetType) (KnownTarget, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.latest[targetType]
	if !ok {
		return KnownTarget{}, false
	}
	return item, true
}

func (s *Store) LatestAny() (KnownTarget, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.hasLast {
		return KnownTarget{}, false
	}
	return s.latestX, true
}

func (s *Store) load() error {
	if _, err := os.Stat(s.filePath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	raw, err := os.ReadFile(s.filePath)
	if err != nil {
		return err
	}
	var list []KnownTarget
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	for _, item := range list {
		key := string(item.TargetType) + ":" + item.TargetID
		s.records[key] = item
	}
	s.rebuildLatestLocked()
	return nil
}

func (s *Store) Close() error {
	s.cancel()
	s.wg.Wait()
	return s.flushNow()
}

func (s *Store) startFlusher() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				_ = s.flushNow()
			case <-s.flushSig:
				_ = s.flushNow()
			}
		}
	}()
}

func (s *Store) flushNow() error {
	records, version, ok := s.snapshotForFlush()
	if !ok {
		return nil
	}
	if err := s.writeRecords(records); err != nil {
		return err
	}
	s.mu.Lock()
	if s.version == version {
		s.dirty = false
	}
	s.mu.Unlock()
	return nil
}

func (s *Store) snapshotForFlush() ([]KnownTarget, uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.dirty {
		return nil, 0, false
	}
	list := make([]KnownTarget, 0, len(s.records))
	for _, item := range s.records {
		list = append(list, item)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].LastSeenAt.After(list[j].LastSeenAt)
	})
	return list, s.version, true
}

func (s *Store) writeRecords(list []KnownTarget) error {
	if err := os.MkdirAll(filepath.Dir(s.filePath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.filePath, data, 0o644)
}

func (s *Store) signalFlushLocked() {
	select {
	case s.flushSig <- struct{}{}:
	default:
	}
}

func (s *Store) compactIfNeededLocked() {
	if len(s.records) <= s.maxRecords {
		return
	}
	list := make([]KnownTarget, 0, len(s.records))
	for _, item := range s.records {
		list = append(list, item)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].LastSeenAt.After(list[j].LastSeenAt)
	})
	cut := list[s.maxRecords:]
	for _, item := range cut {
		key := string(item.TargetType) + ":" + item.TargetID
		delete(s.records, key)
	}
	s.rebuildLatestLocked()
}

func (s *Store) rebuildLatestLocked() {
	clear(s.latest)
	s.latestX = KnownTarget{}
	s.hasLast = false
	for _, item := range s.records {
		prev, ok := s.latest[item.TargetType]
		if !ok || item.LastSeenAt.After(prev.LastSeenAt) {
			s.latest[item.TargetType] = item
		}
		if !s.hasLast || item.LastSeenAt.After(s.latestX.LastSeenAt) {
			s.latestX = item
			s.hasLast = true
		}
	}
}
