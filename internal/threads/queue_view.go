package threads

import (
	"context"

	"github.com/crueber/spindle/internal/server/auth"
)

// QueueSnapshotForRepo reads the repository's pending window in claim order
// (PAR-155's visible ordering). Read role; one head GET plus one GET per
// pending entry (the window is bounded at queueWindowMax).
func (s *Service) QueueSnapshotForRepo(ctx context.Context, repo string, p auth.Principal) (*QueueSnapshot, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, name, p); err != nil {
		return nil, err
	}
	out := &QueueSnapshot{Repo: repoID(owner, name), Rows: []QueueRow{}}
	head, _, herr := s.loadHead(ctx, owner, name)
	if herr != nil {
		return nil, herr
	}
	if head == nil {
		return out, nil
	}
	out.Paused = head.Paused
	for _, seq := range head.Pending {
		entry, _, eerr := s.loadEntry(ctx, owner, name, seq)
		if eerr != nil {
			return nil, eerr
		}
		if entry == nil || entry.State == QueueDone {
			continue
		}
		out.Rows = append(out.Rows, QueueRow{
			Seq: entry.Seq, RunID: entry.RunID, JobID: entry.JobID, Priority: entry.Priority,
			Labels: nonNilStr(entry.Labels), State: entry.State, Fork: entry.Fork,
			Attempt: entry.Attempt, EnqueuedAt: entry.EnqueuedAt,
		})
	}
	return out, nil
}
