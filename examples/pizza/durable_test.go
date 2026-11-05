package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/clitest"
	"github.com/tggo/claude-agent-go/graph"
)

// failCommit loses the commit record of one node: the process "dies" after
// the node's side effect and before the node is committed.
type failCommit struct {
	graph.CheckpointStore
	node string
}

func (f failCommit) Append(ctx context.Context, id string, r graph.Record) error {
	if r.Kind == graph.KindNodeCommit && r.Node == f.node {
		return assert.AnError
	}
	return f.CheckpointStore.Append(ctx, id, r)
}

// The card is charged, then the process dies before the place node commits.
// Recover re-runs place with the same idempotency key (in a "new process":
// another Agent whose shop reloaded its state from disk) and must not charge
// again.
func TestCrashAfterPaymentDoesNotDoubleCharge(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	shopFile := filepath.Join(dir, "shop.json")
	inner := graph.MemStore()
	bin := clitest.FakeCLI(t, clitest.Init("s1"), clitest.Text("Pepperoni, one."), clitest.Result("One pepperoni, 360 UAH.", 0))

	build := func(store graph.CheckpointStore) *Agent {
		a, err := Build(Options{Model: "haiku", RunsDir: t.TempDir(), Binary: bin, Store: store, ShopFile: shopFile})
		require.NoError(t, err)
		t.Cleanup(a.Close)
		return a
	}
	a1 := build(failCommit{inner, "place"})
	// the order node is an agent; the fake cannot call tools, so the draft is
	// what the model would have proposed
	_, err := a1.Shop.Propose("cart-1", []Line{{"pepperoni", Large, 1}}, "Khreshchatyk 22, Kyiv", "")
	require.NoError(t, err)

	_, _, err = a1.Graph.Run(ctx, State{Request: "a pepperoni", CartID: "cart-1"})
	intr, ok := graph.AsInterrupt[State](err)
	require.True(t, ok, "%v", err)
	_, _, err = a1.Graph.Resume(ctx, intr.Checkpoint, "yes")
	require.ErrorIs(t, err, graph.ErrCheckpoint, "the commit of place is lost")
	require.Len(t, a1.Shop.Orders(), 1, "the card was charged")
	runID := intr.Checkpoint.RunID

	a2 := build(inner)
	st, rep, err := a2.Graph.Recover(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, "paid", st.Status)
	assert.NotEmpty(t, st.OrderID)
	orders := a2.Shop.Orders()
	require.Len(t, orders, 1, "no second order, no second charge")
	assert.Equal(t, orders[0].ID, st.OrderID)
	assert.Contains(t, a2.Shop.calls, "place-replayed")
	assert.Equal(t, []string{"order", "confirm", "confirm", "place"}, rep.Path)
	assert.Equal(t, runID, rep.RunID)
}

func TestShopPersistAndIdempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shop.json")
	s := NewShop()
	require.NoError(t, s.Persist(path))
	d, err := s.Propose("c", []Line{{"veggie", Medium, 1}}, "Kyiv", "")
	require.NoError(t, err)
	o1, err := s.PlaceAndPay("c", d.Hash(), "tok_test_visa", "run:place:3")
	require.NoError(t, err)
	o2, err := s.PlaceAndPay("c", d.Hash(), "tok_test_visa", "run:place:3")
	require.NoError(t, err)
	assert.Equal(t, o1, o2)
	assert.Len(t, s.Orders(), 1)

	s2 := NewShop()
	require.NoError(t, s2.Persist(path))
	o3, err := s2.PlaceAndPay("c", d.Hash(), "tok_test_visa", "run:place:3")
	require.NoError(t, err)
	assert.Equal(t, o1, o3, "a reloaded shop remembers the key")
	assert.Len(t, s2.Orders(), 1)

	// another step is another charge, and an empty key is never deduplicated
	d2, _ := s2.Propose("c", []Line{{"veggie", Medium, 1}}, "Kyiv", "")
	o4, err := s2.PlaceAndPay("c", d2.Hash(), "tok_test_visa", "run:place:4")
	require.NoError(t, err)
	assert.NotEqual(t, o1.ID, o4.ID)

	bad := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, writeFile(bad, "{nope"))
	assert.Error(t, NewShop().Persist(bad))
	assert.Error(t, NewShop().Persist(t.TempDir()), "a directory is not a shop file")
}

func TestLastRun(t *testing.T) {
	ctx := context.Background()
	st := graph.MemStore()
	_, err := lastRun(ctx, st)
	assert.Error(t, err)
	require.NoError(t, st.Append(ctx, "done", graph.Record{Seq: 1, Kind: graph.KindRunEnd}))
	_, err = lastRun(ctx, st)
	assert.Error(t, err)
	require.NoError(t, st.Append(ctx, "open", graph.Record{Seq: 1, Kind: graph.KindRunStart}))
	id, err := lastRun(ctx, st)
	require.NoError(t, err)
	assert.Equal(t, "open", id)
}

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }
