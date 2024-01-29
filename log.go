// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"fmt"
	"log"

	pb "go.etcd.io/raft/v3/raftpb"
)

type raftLog struct {
	// storage contains all stable entries since the last snapshot.
	storage Storage

	// unstable contains all unstable entries and snapshot.
	// they will be saved into storage.
	unstable unstable

	// leaderTerm is a term of the leader with whom our log is "consistent". The
	// log is guaranteed to be a prefix of this term's leader log.
	//
	// The leaderTerm can be safely updated to `t` if:
	//	1. the last entry in the log has term `t`, or, more generally,
	//	2. the last successful append was sent by the leader `t`.
	//
	// This is due to the following safety property (see raft paper §5.3):
	//
	//	Log Matching: if two logs contain an entry with the same index and term,
	//	then the logs are identical in all entries up through the given index.
	//
	// We use (1) to initialize leaderTerm, and (2) to maintain it on updates.
	//
	// NB: (2) does not imply (1). If our log is behind the leader's log, the last
	// entry term can be below leaderTerm.
	//
	// NB: leaderTerm does not necessarily match this raft node's term. It only
	// does for the leader. For followers and candidates, when we first learn or
	// bump to a new term, we don't have a proof that our log is consistent with
	// the new term's leader (current or prospective). The new leader may override
	// any suffix of the log after the committed index. Only when the first append
	// from the new leader succeeds, we can update leaderTerm.
	//
	// During normal operation, leaderTerm matches the node term though. During a
	// leader change, it briefly lags behind, and matches again when the first
	// append message succeeds.
	leaderTerm uint64

	// committed is the highest log position that is known to be in
	// stable storage on a quorum of nodes.
	committed uint64
	// applying is the highest log position that the application has
	// been instructed to apply to its state machine. Some of these
	// entries may be in the process of applying and have not yet
	// reached applied.
	// Use: The field is incremented when accepting a Ready struct.
	// Invariant: applied <= applying && applying <= committed
	applying uint64
	// applied is the highest log position that the application has
	// successfully applied to its state machine.
	// Use: The field is incremented when advancing after the committed
	// entries in a Ready struct have been applied (either synchronously
	// or asynchronously).
	// Invariant: applied <= committed
	applied uint64

	logger Logger

	// maxApplyingEntsSize limits the outstanding byte size of the messages
	// returned from calls to nextCommittedEnts that have not been acknowledged
	// by a call to appliedTo.
	maxApplyingEntsSize entryEncodingSize
	// applyingEntsSize is the current outstanding byte size of the messages
	// returned from calls to nextCommittedEnts that have not been acknowledged
	// by a call to appliedTo.
	applyingEntsSize entryEncodingSize
	// applyingEntsPaused is true when entry application has been paused until
	// enough progress is acknowledged.
	applyingEntsPaused bool
}

// newLog returns log using the given storage and default options. It
// recovers the log to the state that it just commits and applies the
// latest snapshot.
func newLog(storage Storage, logger Logger) *raftLog {
	return newLogWithSize(storage, logger, noLimit)
}

// newLogWithSize returns a log using the given storage and max
// message size.
func newLogWithSize(storage Storage, logger Logger, maxApplyingEntsSize entryEncodingSize) *raftLog {
	if storage == nil {
		log.Panic("storage must not be nil")
	}
	log := &raftLog{
		storage:             storage,
		logger:              logger,
		maxApplyingEntsSize: maxApplyingEntsSize,
	}
	firstIndex, err := storage.FirstIndex()
	if err != nil {
		panic(err) // TODO(bdarnell)
	}
	lastIndex, err := storage.LastIndex()
	if err != nil {
		panic(err) // TODO(bdarnell)
	}
	lastTerm, err := storage.Term(lastIndex)
	if err != nil {
		panic(err) // TODO(pav-kv)
	}
	log.leaderTerm = lastTerm
	log.unstable.offset = lastIndex + 1
	log.unstable.offsetInProgress = lastIndex + 1
	log.unstable.logger = logger
	// Initialize our committed and applied pointers to the time of the last compaction.
	log.committed = firstIndex - 1
	log.applying = firstIndex - 1
	log.applied = firstIndex - 1

	return log
}

func (l *raftLog) String() string {
	return fmt.Sprintf("committed=%d, applied=%d, applying=%d, unstable.offset=%d, unstable.offsetInProgress=%d, len(unstable.Entries)=%d",
		l.committed, l.applied, l.applying, l.unstable.offset, l.unstable.offsetInProgress, len(l.unstable.entries))
}

// maybeAppend returns (0, false) if the entries cannot be appended. Otherwise,
// it returns (last index of new entries, true).
//
// TODO(pav-kv): introduce a struct that consolidates the append metadata. The
// (leaderTerm, prevIndex, prevTerm) tuple must always be carried together, so
// that safety properties for this append are checked at the lowest layers
// rather than up in raft.go.
func (l *raftLog) maybeAppend(leaderTerm, prevIndex, prevTerm, committed uint64, ents ...pb.Entry) (lastnewi uint64, ok bool) {
	// Can not accept append requests from an outdated leader.
	if leaderTerm < l.leaderTerm {
		return 0, false
	}
	// Can not accept append requests that are not consistent with our log.
	//
	// NB: it is unnecessary to check matchTerm() if leaderTerm == l.leaderTerm,
	// because the leader always sends self-consistent appends. For ensuring raft
	// safety, this check is only necessary if leaderTerm > l.leaderTerm.
	//
	// TODO(pav-kv): however, we should log an error if leaderTerm == l.leaderTerm
	// and the entry does not match. This means either the leader is sending
	// inconsistent appends, or there is some state corruption in general.
	if !l.matchTerm(prevIndex, prevTerm) {
		return 0, false
	}

	lastnewi = prevIndex + uint64(len(ents))
	ci := l.findConflict(ents)
	switch {
	case ci == 0:
	case ci <= l.committed:
		l.logger.Panicf("entry %d conflict with committed entry [committed(%d)]", ci, l.committed)
	default:
		offset := prevIndex + 1
		if ci-offset > uint64(len(ents)) {
			l.logger.Panicf("index, %d, is out of range [%d]", ci-offset, len(ents))
		}
		l.append(leaderTerm, ents[ci-offset:]...)
	}
	// TODO(pav-kv): call commitTo from outside of this method, for a smaller API.
	// TODO(pav-kv): it is safe to pass committed index as is here instead of min,
	// but it breaks some tests that make incorrect assumptions. Fix this.
	l.commitTo(leaderTerm, min(committed, lastnewi))
	return lastnewi, true
}

func (l *raftLog) append(leaderTerm uint64, ents ...pb.Entry) uint64 {
	// Can not accept append requests from an outdated leader.
	if leaderTerm < l.leaderTerm {
		return l.lastIndex()
	}
	if len(ents) == 0 { // no-op
		return l.lastIndex()
	}
	if after := ents[0].Index - 1; after < l.committed {
		l.logger.Panicf("after(%d) is out of range [committed(%d)]", after, l.committed)
	}

	// INVARIANT: l.term(i) <= l.leaderTerm, for any entry in the log.
	//
	// TODO(pav-kv): we should more generally check that the content of ents slice
	// is correct: all entries have consecutive indices, and terms do not regress.
	// We should do this validation once, on every incoming message, and pass the
	// append in a type-safe "validated append" wrapper. This wrapper can provide
	// convenient accessors to the prev/last entry, instead of raw slices access.
	if lastTerm := ents[len(ents)-1].Term; lastTerm > leaderTerm {
		l.logger.Panicf("leader at term %d tries to append a higher term %d", leaderTerm, lastTerm)
	}
	l.leaderTerm = leaderTerm // l.leaderTerm never regresses here

	l.unstable.truncateAndAppend(ents)
	return l.lastIndex()
}

// findConflict finds the index of the conflict.
// It returns the first pair of conflicting entries between the existing
// entries and the given entries, if there are any.
// If there is no conflicting entries, and the existing entries contains
// all the given entries, zero will be returned.
// If there is no conflicting entries, but the given entries contains new
// entries, the index of the first new entry will be returned.
// An entry is considered to be conflicting if it has the same index but
// a different term.
// The index of the given entries MUST be continuously increasing.
func (l *raftLog) findConflict(ents []pb.Entry) uint64 {
	for _, ne := range ents {
		if !l.matchTerm(ne.Index, ne.Term) {
			if ne.Index <= l.lastIndex() {
				l.logger.Infof("found conflict at index %d [existing term: %d, conflicting term: %d]",
					ne.Index, l.zeroTermOnOutOfBounds(l.term(ne.Index)), ne.Term)
			}
			return ne.Index
		}
	}
	return 0
}

// findConflictByTerm returns a best guess on where this log ends matching
// another log, given that the only information known about the other log is the
// (index, term) of its single entry.
//
// Specifically, the first returned value is the max guessIndex <= index, such
// that term(guessIndex) <= term or term(guessIndex) is not known (because this
// index is compacted or not yet stored).
//
// The second returned value is the term(guessIndex), or 0 if it is unknown.
//
// This function is used by a follower and leader to resolve log conflicts after
// an unsuccessful append to a follower, and ultimately restore the steady flow
// of appends.
func (l *raftLog) findConflictByTerm(index uint64, term uint64) (uint64, uint64) {
	for ; index > 0; index-- {
		// If there is an error (likely ErrCompacted or ErrUnavailable), we don't
		// know whether it's a match or not, so assume a possible match and return
		// the index, with 0 term indicating an unknown term.
		if ourTerm, err := l.term(index); err != nil {
			return index, 0
		} else if ourTerm <= term {
			return index, ourTerm
		}
	}
	return 0, 0
}

// nextUnstableEnts returns all entries that are available to be written to the
// local stable log and are not already in-progress.
func (l *raftLog) nextUnstableEnts() []pb.Entry {
	return l.unstable.nextEntries()
}

// hasNextUnstableEnts returns if there are any entries that are available to be
// written to the local stable log and are not already in-progress.
func (l *raftLog) hasNextUnstableEnts() bool {
	return len(l.nextUnstableEnts()) > 0
}

// hasNextOrInProgressUnstableEnts returns if there are any entries that are
// available to be written to the local stable log or in the process of being
// written to the local stable log.
func (l *raftLog) hasNextOrInProgressUnstableEnts() bool {
	return len(l.unstable.entries) > 0
}

// nextCommittedEnts returns all the available entries for execution.
// Entries can be committed even when the local raft instance has not durably
// appended them to the local raft log yet. If allowUnstable is true, committed
// entries from the unstable log may be returned; otherwise, only entries known
// to reside locally on stable storage will be returned.
func (l *raftLog) nextCommittedEnts(allowUnstable bool) (ents []pb.Entry) {
	if l.applyingEntsPaused {
		// Entry application outstanding size limit reached.
		return nil
	}
	if l.hasNextOrInProgressSnapshot() {
		// See comment in hasNextCommittedEnts.
		return nil
	}
	lo, hi := l.applying+1, l.maxAppliableIndex(allowUnstable)+1 // [lo, hi)
	if lo >= hi {
		// Nothing to apply.
		return nil
	}
	maxSize := l.maxApplyingEntsSize - l.applyingEntsSize
	if maxSize <= 0 {
		l.logger.Panicf("applying entry size (%d-%d)=%d not positive",
			l.maxApplyingEntsSize, l.applyingEntsSize, maxSize)
	}
	ents, err := l.slice(lo, hi, maxSize)
	if err != nil {
		l.logger.Panicf("unexpected error when getting unapplied entries (%v)", err)
	}
	return ents
}

// hasNextCommittedEnts returns if there is any available entries for execution.
// This is a fast check without heavy raftLog.slice() in nextCommittedEnts().
func (l *raftLog) hasNextCommittedEnts(allowUnstable bool) bool {
	if l.applyingEntsPaused {
		// Entry application outstanding size limit reached.
		return false
	}
	if l.hasNextOrInProgressSnapshot() {
		// If we have a snapshot to apply, don't also return any committed
		// entries. Doing so raises questions about what should be applied
		// first.
		return false
	}
	lo, hi := l.applying+1, l.maxAppliableIndex(allowUnstable)+1 // [lo, hi)
	return lo < hi
}

// maxAppliableIndex returns the maximum committed index that can be applied.
// If allowUnstable is true, committed entries from the unstable log can be
// applied; otherwise, only entries known to reside locally on stable storage
// can be applied.
func (l *raftLog) maxAppliableIndex(allowUnstable bool) uint64 {
	hi := l.committed
	if !allowUnstable {
		hi = min(hi, l.unstable.offset-1)
	}
	return hi
}

// nextUnstableSnapshot returns the snapshot, if present, that is available to
// be applied to the local storage and is not already in-progress.
func (l *raftLog) nextUnstableSnapshot() *pb.Snapshot {
	return l.unstable.nextSnapshot()
}

// hasNextUnstableSnapshot returns if there is a snapshot that is available to
// be applied to the local storage and is not already in-progress.
func (l *raftLog) hasNextUnstableSnapshot() bool {
	return l.unstable.nextSnapshot() != nil
}

// hasNextOrInProgressSnapshot returns if there is pending snapshot waiting for
// applying or in the process of being applied.
func (l *raftLog) hasNextOrInProgressSnapshot() bool {
	return l.unstable.snapshot != nil
}

func (l *raftLog) snapshot() (pb.Snapshot, error) {
	if l.unstable.snapshot != nil {
		return *l.unstable.snapshot, nil
	}
	return l.storage.Snapshot()
}

func (l *raftLog) firstIndex() uint64 {
	if i, ok := l.unstable.maybeFirstIndex(); ok {
		return i
	}
	index, err := l.storage.FirstIndex()
	if err != nil {
		panic(err) // TODO(bdarnell)
	}
	return index
}

func (l *raftLog) lastIndex() uint64 {
	if i, ok := l.unstable.maybeLastIndex(); ok {
		return i
	}
	i, err := l.storage.LastIndex()
	if err != nil {
		panic(err) // TODO(bdarnell)
	}
	return i
}

func (l *raftLog) commitTo(leaderTerm, tocommit uint64) {
	// Do not accept the commit index update from a leader if our log is not
	// consistent with the leader's log.
	if leaderTerm != l.leaderTerm {
		return
	}
	// Otherwise, we have the guarantee that our log is a prefix of the leader's
	// log. All entries <= min(tocommit, lastIndex) can thus be committed.
	tocommit = min(tocommit, l.lastIndex())
	if tocommit > l.committed {
		l.committed = tocommit
	}
}

func (l *raftLog) appliedTo(i uint64, size entryEncodingSize) {
	if l.committed < i || i < l.applied {
		l.logger.Panicf("applied(%d) is out of range [prevApplied(%d), committed(%d)]", i, l.applied, l.committed)
	}
	l.applied = i
	l.applying = max(l.applying, i)
	if l.applyingEntsSize > size {
		l.applyingEntsSize -= size
	} else {
		// Defense against underflow.
		l.applyingEntsSize = 0
	}
	l.applyingEntsPaused = l.applyingEntsSize >= l.maxApplyingEntsSize
}

func (l *raftLog) acceptApplying(i uint64, size entryEncodingSize, allowUnstable bool) {
	if l.committed < i {
		l.logger.Panicf("applying(%d) is out of range [prevApplying(%d), committed(%d)]", i, l.applying, l.committed)
	}
	l.applying = i
	l.applyingEntsSize += size
	// Determine whether to pause entry application until some progress is
	// acknowledged. We pause in two cases:
	// 1. the outstanding entry size equals or exceeds the maximum size.
	// 2. the outstanding entry size does not equal or exceed the maximum size,
	//    but we determine that the next entry in the log will push us over the
	//    limit. We determine this by comparing the last entry returned from
	//    raftLog.nextCommittedEnts to the maximum entry that the method was
	//    allowed to return had there been no size limit. If these indexes are
	//    not equal, then the returned entries slice must have been truncated to
	//    adhere to the memory limit.
	l.applyingEntsPaused = l.applyingEntsSize >= l.maxApplyingEntsSize ||
		i < l.maxAppliableIndex(allowUnstable)
}

func (l *raftLog) stableTo(i, t uint64) { l.unstable.stableTo(i, t) }

func (l *raftLog) stableSnapTo(i uint64) { l.unstable.stableSnapTo(i) }

// acceptUnstable indicates that the application has started persisting the
// unstable entries in storage, and that the current unstable entries are thus
// to be marked as being in-progress, to avoid returning them with future calls
// to Ready().
func (l *raftLog) acceptUnstable() { l.unstable.acceptInProgress() }

func (l *raftLog) lastTerm() uint64 {
	t, err := l.term(l.lastIndex())
	if err != nil {
		l.logger.Panicf("unexpected error when getting the last term (%v)", err)
	}
	return t
}

func (l *raftLog) term(i uint64) (uint64, error) {
	// Check the unstable log first, even before computing the valid term range,
	// which may need to access stable Storage. If we find the entry's term in
	// the unstable log, we know it was in the valid range.
	if t, ok := l.unstable.maybeTerm(i); ok {
		return t, nil
	}

	// The valid term range is [firstIndex-1, lastIndex]. Even though the entry at
	// firstIndex-1 is compacted away, its term is available for matching purposes
	// when doing log appends.
	if i+1 < l.firstIndex() {
		return 0, ErrCompacted
	}
	if i > l.lastIndex() {
		return 0, ErrUnavailable
	}

	t, err := l.storage.Term(i)
	if err == nil {
		return t, nil
	}
	if err == ErrCompacted || err == ErrUnavailable {
		return 0, err
	}
	panic(err) // TODO(bdarnell)
}

func (l *raftLog) entries(i uint64, maxSize entryEncodingSize) ([]pb.Entry, error) {
	if i > l.lastIndex() {
		return nil, nil
	}
	return l.slice(i, l.lastIndex()+1, maxSize)
}

// allEntries returns all entries in the log.
func (l *raftLog) allEntries() []pb.Entry {
	ents, err := l.entries(l.firstIndex(), noLimit)
	if err == nil {
		return ents
	}
	if err == ErrCompacted { // try again if there was a racing compaction
		return l.allEntries()
	}
	// TODO (xiangli): handle error?
	panic(err)
}

// isUpToDate determines if the given (lastIndex,term) log is more up-to-date
// by comparing the index and term of the last entries in the existing logs.
// If the logs have last entries with different terms, then the log with the
// later term is more up-to-date. If the logs end with the same term, then
// whichever log has the larger lastIndex is more up-to-date. If the logs are
// the same, the given log is up-to-date.
func (l *raftLog) isUpToDate(lasti, term uint64) bool {
	return term > l.lastTerm() || (term == l.lastTerm() && lasti >= l.lastIndex())
}

func (l *raftLog) matchTerm(i, term uint64) bool {
	t, err := l.term(i)
	if err != nil {
		return false
	}
	return t == term
}

// TODO(pav-kv): clarify that (maxIndex, term) is the ID of the entry at the
// committed index. Clean this up.
func (l *raftLog) maybeCommit(leaderTerm, maxIndex, term uint64) bool {
	// NB: term should never be 0 on a commit because the leader campaigns at
	// least at term 1. But if it is 0 for some reason, we don't want to consider
	// this a term match in case zeroTermOnOutOfBounds returns 0.
	if maxIndex > l.committed && term != 0 && l.zeroTermOnOutOfBounds(l.term(maxIndex)) == term {
		l.commitTo(leaderTerm, maxIndex)
		return true
	}
	return false
}

func (l *raftLog) restore(s pb.Snapshot) {
	l.logger.Infof("log [%s] starts to restore snapshot [index: %d, term: %d]", l, s.Metadata.Index, s.Metadata.Term)
	l.committed = s.Metadata.Index
	l.unstable.restore(s)
}

// scan visits all log entries in the [lo, hi) range, returning them via the
// given callback. The callback can be invoked multiple times, with consecutive
// sub-ranges of the requested range. Returns up to pageSize bytes worth of
// entries at a time. May return more if a single entry size exceeds the limit.
//
// The entries in [lo, hi) must exist, otherwise scan() eventually returns an
// error (possibly after passing some entries through the callback).
//
// If the callback returns an error, scan terminates and returns this error
// immediately. This can be used to stop the scan early ("break" the loop).
func (l *raftLog) scan(lo, hi uint64, pageSize entryEncodingSize, v func([]pb.Entry) error) error {
	for lo < hi {
		ents, err := l.slice(lo, hi, pageSize)
		if err != nil {
			return err
		} else if len(ents) == 0 {
			return fmt.Errorf("got 0 entries in [%d, %d)", lo, hi)
		}
		if err := v(ents); err != nil {
			return err
		}
		lo += uint64(len(ents))
	}
	return nil
}

// slice returns a slice of log entries from lo through hi-1, inclusive.
func (l *raftLog) slice(lo, hi uint64, maxSize entryEncodingSize) ([]pb.Entry, error) {
	if err := l.mustCheckOutOfBounds(lo, hi); err != nil {
		return nil, err
	}
	if lo == hi {
		return nil, nil
	}
	if lo >= l.unstable.offset {
		ents := limitSize(l.unstable.slice(lo, hi), maxSize)
		// NB: use the full slice expression to protect the unstable slice from
		// appends to the returned ents slice.
		return ents[:len(ents):len(ents)], nil
	}

	cut := min(hi, l.unstable.offset)
	ents, err := l.storage.Entries(lo, cut, uint64(maxSize))
	if err == ErrCompacted {
		return nil, err
	} else if err == ErrUnavailable {
		l.logger.Panicf("entries[%d:%d) is unavailable from storage", lo, cut)
	} else if err != nil {
		panic(err) // TODO(pavelkalinnikov): handle errors uniformly
	}
	if hi <= l.unstable.offset {
		return ents, nil
	}

	// Fast path to check if ents has reached the size limitation. Either the
	// returned slice is shorter than requested (which means the next entry would
	// bring it over the limit), or a single entry reaches the limit.
	if uint64(len(ents)) < cut-lo {
		return ents, nil
	}
	// Slow path computes the actual total size, so that unstable entries are cut
	// optimally before being copied to ents slice.
	size := entsSize(ents)
	if size >= maxSize {
		return ents, nil
	}

	unstable := limitSize(l.unstable.slice(l.unstable.offset, hi), maxSize-size)
	// Total size of unstable may exceed maxSize-size only if len(unstable) == 1.
	// If this happens, ignore this extra entry.
	if len(unstable) == 1 && size+entsSize(unstable) > maxSize {
		return ents, nil
	}
	// Otherwise, total size of unstable does not exceed maxSize-size, so total
	// size of ents+unstable does not exceed maxSize. Simply concatenate them.
	return extend(ents, unstable), nil
}

// l.firstIndex <= lo <= hi <= l.firstIndex + len(l.entries)
func (l *raftLog) mustCheckOutOfBounds(lo, hi uint64) error {
	if lo > hi {
		l.logger.Panicf("invalid slice %d > %d", lo, hi)
	}
	fi := l.firstIndex()
	if lo < fi {
		return ErrCompacted
	}

	length := l.lastIndex() + 1 - fi
	if hi > fi+length {
		l.logger.Panicf("slice[%d,%d) out of bound [%d,%d]", lo, hi, fi, l.lastIndex())
	}
	return nil
}

func (l *raftLog) zeroTermOnOutOfBounds(t uint64, err error) uint64 {
	if err == nil {
		return t
	}
	if err == ErrCompacted || err == ErrUnavailable {
		return 0
	}
	l.logger.Panicf("unexpected error (%v)", err)
	return 0
}
