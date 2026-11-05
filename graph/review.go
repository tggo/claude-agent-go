package graph

import (
	"context"
	"encoding/json"
)

// ReviewItem is one thing a human is asked to look at (usually an image).
type ReviewItem struct {
	Name    string `json:"name"` // e.g. "p05"
	Path    string `json:"path"` // file to show (image usually)
	Caption string `json:"caption,omitempty"`
}

// Review is the human's verdict on a set of ReviewItems.
type Review struct {
	Approved bool              `json:"approved"`           // operator pressed "approve all"
	Comments map[string]string `json:"comments,omitempty"` // item name -> comment (only non-empty)
	General  string            `json:"general,omitempty"`  // free comment for the whole thing
}

// InterruptReview returns the error a node returns to pause and ask for a review of items.
// On resume, ReviewAnswer(ctx) yields the parsed Review.
//
// It is an ordinary Interrupt: errors.Is(err, ErrInterrupt) holds, Run returns
// an *Interrupted whose Items lists the items, and the answer given to Resume
// is the JSON encoding of a Review, so a plain Resume(ctx, cp, string(json))
// works from any front end. Relative item paths are not resolved; pass
// absolute paths.
func InterruptReview(question string, items []ReviewItem) error {
	return &interruptError{Question: question, Items: append([]ReviewItem(nil), items...)}
}

// ReviewAnswer parses the answer of the Resume that is re-running the current
// node as a Review. It reports false on an ordinary execution. An answer that
// is not a JSON Review (typed by hand) becomes General, so it is not lost.
func ReviewAnswer(ctx context.Context) (Review, bool) {
	a, ok := Answer(ctx)
	if !ok {
		return Review{}, false
	}
	var r Review
	if json.Unmarshal([]byte(a), &r) != nil {
		return Review{General: a}, true
	}
	return r, true
}
