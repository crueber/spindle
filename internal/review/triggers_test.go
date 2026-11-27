package review

// PAR-020: the review service's trigger edges. The parser and the threads side
// are pinned elsewhere; here the service must emit only edges it really makes.

import (
	"context"
	"testing"
)

func TestReviewTriggerEdges(t *testing.T) {
	ctx := context.Background()
	svc, _ := testSvc()
	seedPR(t, svc)
	var reviews []ReviewTriggerEvent
	var comments []ReviewCommentTriggerEvent
	svc.ReviewEvents = func(_ context.Context, ev ReviewTriggerEvent) { reviews = append(reviews, ev) }
	svc.ReviewCommentEvents = func(_ context.Context, ev ReviewCommentTriggerEvent) { comments = append(comments, ev) }

	if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), SubmitInput{
		State: StateCommented, CommitSHA: testHead,
		Threads: []NewThread{{Anchor: testAnchor(), Body: "nit"}},
	}); err != nil {
		t.Fatalf("SubmitReview: %v", err)
	}
	if len(reviews) != 1 || reviews[0].Action != "submitted" || reviews[0].State != "commented" ||
		reviews[0].Num != testPR || reviews[0].Reviewer != "bob" || reviews[0].Actor != "bob" {
		t.Fatalf("submitted review = %+v", reviews)
	}
	if len(comments) != 1 || comments[0].Action != "created" || comments[0].Body != "nit" ||
		comments[0].Num != testPR || comments[0].Anchor.Path == "" {
		t.Fatalf("inline comment = %+v", comments)
	}

	// A reply on the thread is another `created` review-comment edge.
	svc.ReviewCommentEvents = func(_ context.Context, ev ReviewCommentTriggerEvent) { comments = append(comments, ev) }
	comments = nil
	if _, err := svc.AddThreadComment(ctx, testOwner, testRepo, testPR, "00000001", testPrincipal("carol"), "reply"); err != nil {
		t.Fatalf("AddThreadComment: %v", err)
	}
	if len(comments) != 1 || comments[0].Body != "reply" || comments[0].TID != "00000001" {
		t.Fatalf("reply edge = %+v", comments)
	}

	// Dismissal is the second review action.
	reviews = nil
	if _, _, err := svc.DismissReview(ctx, testOwner, testRepo, testPR, 1, testPrincipal("bob"), "stale"); err != nil {
		t.Fatalf("DismissReview: %v", err)
	}
	if len(reviews) != 1 || reviews[0].Action != "dismissed" || reviews[0].State != "dismissed" ||
		reviews[0].Reviewer != "bob" || reviews[0].Actor != "bob" {
		t.Fatalf("dismissed review = %+v", reviews)
	}

	// Nil sinks are safe (a Service built without composition).
	svc.ReviewEvents = nil
	svc.ReviewCommentEvents = nil
	if _, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "another"); err != nil {
		t.Fatalf("OpenThread with nil sinks: %v", err)
	}
}
