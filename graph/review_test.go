package graph

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type revObs struct {
	artObs
	paused []StepInfo
}

func (o *revObs) NodeEnd(_ context.Context, i StepInfo, _ error) {
	if i.Paused {
		o.paused = append(o.paused, i)
	}
}

func TestReviewRoundTrip(t *testing.T) {
	items := []ReviewItem{{Name: "p01", Path: "/a/p01.png", Caption: "one"}, {Name: "p02", Path: "/a/p02.png"}}
	var got Review
	var ok, ordinary bool
	g := New[string]("rev").
		AddNode("letter", func(ctx context.Context, s *string) error {
			r, isAns := ReviewAnswer(ctx)
			if !isAns {
				_, ordinary = ReviewAnswer(ctx)
				return InterruptReview("check the panels", items)
			}
			got, ok = r, true
			*s = r.General
			return nil
		}).AddEdge(Start, "letter").AddEdge("letter", End)
	obs := &revObs{}
	r, err := g.Compile(WithObserver(obs))
	require.NoError(t, err)

	_, _, err = r.Run(context.Background(), "")
	require.ErrorIs(t, err, ErrInterrupt)
	assert.False(t, ordinary)
	ie, isInt := AsInterrupt[string](err)
	require.True(t, isInt)
	assert.Equal(t, "check the panels", ie.Question)
	assert.Equal(t, items, ie.Items)
	require.Len(t, obs.paused, 1)
	assert.Equal(t, items, obs.paused[0].Items)

	// The checkpoint is plain data; the answer is the JSON of a Review.
	cpJSON, err := json.Marshal(ie.Checkpoint)
	require.NoError(t, err)
	var cp Checkpoint[string]
	require.NoError(t, json.Unmarshal(cpJSON, &cp))
	ans, _ := json.Marshal(Review{Comments: map[string]string{"p02": "bubble too big"}, General: "ok overall"})
	final, _, err := r.Resume(context.Background(), cp, string(ans))
	require.NoError(t, err)
	require.True(t, ok)
	assert.False(t, got.Approved)
	assert.Equal(t, "bubble too big", got.Comments["p02"])
	assert.Equal(t, "ok overall", final)
}

func TestReviewAnswerPlainTextAndOrdinary(t *testing.T) {
	_, ok := ReviewAnswer(context.Background())
	assert.False(t, ok)
	r, ok := ReviewAnswer(context.WithValue(context.Background(), answerKey{}, "looks good"))
	assert.True(t, ok)
	assert.Equal(t, Review{General: "looks good"}, r)
	assert.False(t, errors.Is(InterruptReview("q", nil), context.Canceled))
	assert.ErrorIs(t, InterruptReview("q", nil), ErrInterrupt)
}
