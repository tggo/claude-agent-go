package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// This file is the simulated outside world: a pizzeria menu, a delivery
// service and a payment gateway. Everything is in memory and deterministic,
// so tests can assert exactly what the agent ordered. Failures (out of stock,
// address outside the zone, declined card) are injectable.

// Size multiplies the base price.
type Size string

const (
	Small  Size = "S"
	Medium Size = "M"
	Large  Size = "L"
)

var sizeFactor = map[Size]int{Small: 80, Medium: 100, Large: 130} // percent

// MenuItem is one pizza. Prices are in kopecks (UAH/100) to stay integral.
type MenuItem struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Ingredients []string `json:"ingredients"`
	Vegetarian  bool     `json:"vegetarian"`
	BasePrice   int      `json:"base_price_kop"` // size M
	InStock     bool     `json:"in_stock"`
}

// Line is one ordered position.
type Line struct {
	ItemID string `json:"item_id" jsonschema:"menu item id, e.g. pepperoni"`
	Size   Size   `json:"size" jsonschema:"S, M or L"`
	Qty    int    `json:"qty" jsonschema:"quantity, 1..10"`
}

// Draft is a priced, validated order that has not been placed yet.
type Draft struct {
	CartID      string `json:"cart_id"`
	Lines       []Line `json:"lines"`
	Address     string `json:"address"`
	Notes       string `json:"notes,omitempty"`
	ItemsKop    int    `json:"items_kop"`
	DeliveryKop int    `json:"delivery_kop"`
	TotalKop    int    `json:"total_kop"`
	ETAMinutes  int    `json:"eta_minutes"`
}

// Hash is the sha256 (hex) of the draft's canonical JSON (struct field order is
// fixed), used to bind a human confirmation to exactly this draft.
func (d Draft) Hash() string {
	b, _ := json.Marshal(d)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ErrDraftChanged is returned when the stored draft is not the one confirmed.
var ErrDraftChanged = errors.New("draft changed after confirmation")

// Order is a placed and paid order.
type Order struct {
	ID     string `json:"id"`
	Draft  Draft  `json:"draft"`
	Status string `json:"status"` // paid | payment_failed
	Charge string `json:"charge_id,omitempty"`
}

// Shop holds the mocks' state.
type Shop struct {
	mu       sync.Mutex
	menu     map[string]*MenuItem
	drafts   map[string]Draft // by cart id, latest proposal wins
	orders   []Order
	declined bool // next charge is declined
	calls    []string
	byKey    map[string]Order // paid orders by idempotency key
	path     string           // when set, drafts and orders are saved here after every change
}

// shopFile is what Persist keeps: the shop's side of the world, which must
// outlive the process for a crashed run to be recoverable (a real shop and
// payment gateway are somebody else's database).
type shopFile struct {
	Drafts map[string]Draft `json:"drafts"`
	Orders []Order          `json:"orders"`
	ByKey  map[string]Order `json:"by_key"`
}

// Persist loads the shop's drafts and orders from path (if it exists) and
// saves them there after every change, standing in for the external services
// that keep state across our process dying.
func (s *Shop) Persist(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = path
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f shopFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("shop file %s: %w", path, err)
	}
	if f.Drafts != nil {
		s.drafts = f.Drafts
	}
	s.orders = f.Orders
	if f.ByKey != nil {
		s.byKey = f.ByKey
	}
	return nil
}

// save writes the snapshot; the caller holds s.mu.
func (s *Shop) save() {
	if s.path == "" {
		return
	}
	b, _ := json.Marshal(shopFile{Drafts: s.drafts, Orders: s.orders, ByKey: s.byKey})
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// NewShop returns a shop with the standard menu, everything in stock.
func NewShop() *Shop {
	s := &Shop{menu: map[string]*MenuItem{}, drafts: map[string]Draft{}, byKey: map[string]Order{}}
	for _, m := range []MenuItem{
		{ID: "margherita", Name: "Margherita", Ingredients: []string{"tomato", "mozzarella", "basil"}, Vegetarian: true, BasePrice: 18000},
		{ID: "pepperoni", Name: "Pepperoni", Ingredients: []string{"tomato", "mozzarella", "pepperoni"}, BasePrice: 22000},
		{ID: "quattro", Name: "Quattro Formaggi", Ingredients: []string{"mozzarella", "gorgonzola", "parmesan", "dor blue"}, Vegetarian: true, BasePrice: 25000},
		{ID: "veggie", Name: "Veggie", Ingredients: []string{"tomato", "pepper", "mushrooms", "olives", "onion"}, Vegetarian: true, BasePrice: 20000},
		{ID: "diavola", Name: "Diavola", Ingredients: []string{"tomato", "mozzarella", "spicy salami", "chili"}, BasePrice: 23000},
	} {
		m.InStock = true
		s.menu[m.ID] = &m
	}
	return s
}

// SetOutOfStock marks an item unavailable (failure injection).
func (s *Shop) SetOutOfStock(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.menu[id]; ok {
		m.InStock = false
	}
}

// DeclineNextCharge makes the next payment fail (failure injection).
func (s *Shop) DeclineNextCharge() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.declined = true
}

func (s *Shop) log(call string) { s.calls = append(s.calls, call) }

// Menu lists all items, sorted by id.
func (s *Shop) Menu() []MenuItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log("menu")
	out := make([]MenuItem, 0, len(s.menu))
	for _, m := range s.menu {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ErrNotDeliverable is returned for addresses outside the delivery zone.
var ErrNotDeliverable = errors.New("address is outside the delivery zone (we deliver in Kyiv only)")

// QuoteDelivery returns the fee and ETA for an address. Only Kyiv is served;
// the left bank costs more.
func (s *Shop) QuoteDelivery(address string) (feeKop, etaMin int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log("quote:" + address)
	a := strings.ToLower(address)
	if !strings.Contains(a, "kyiv") && !strings.Contains(a, "київ") && !strings.Contains(a, "kiev") {
		return 0, 0, ErrNotDeliverable
	}
	for _, left := range []string{"obolon", "troieshchyna", "darnytsia", "poznyaky", "оболонь", "троєщина", "дарниця", "позняки"} {
		if strings.Contains(a, left) {
			return 9000, 55, nil
		}
	}
	return 6000, 35, nil
}

// Propose validates and prices an order and stores it as the cart's draft.
func (s *Shop) Propose(cartID string, lines []Line, address, notes string) (Draft, error) {
	if cartID == "" {
		return Draft{}, errors.New("cart_id is required")
	}
	if len(lines) == 0 {
		return Draft{}, errors.New("order has no lines")
	}
	fee, eta, err := s.QuoteDelivery(address)
	if err != nil {
		return Draft{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log("propose")
	d := Draft{CartID: cartID, Address: address, Notes: notes, DeliveryKop: fee, ETAMinutes: eta}
	for _, l := range lines {
		m, ok := s.menu[l.ItemID]
		if !ok {
			return Draft{}, fmt.Errorf("unknown item %q", l.ItemID)
		}
		if !m.InStock {
			return Draft{}, fmt.Errorf("%s is out of stock today", m.Name)
		}
		f, ok := sizeFactor[l.Size]
		if !ok {
			return Draft{}, fmt.Errorf("size must be S, M or L, got %q", l.Size)
		}
		if l.Qty < 1 || l.Qty > 10 {
			return Draft{}, fmt.Errorf("qty must be 1..10, got %d", l.Qty)
		}
		d.ItemsKop += m.BasePrice * f / 100 * l.Qty
		d.Lines = append(d.Lines, l)
	}
	d.TotalKop = d.ItemsKop + d.DeliveryKop
	s.drafts[cartID] = d
	s.save()
	return d, nil
}

// Draft returns the cart's latest proposal.
func (s *Shop) Draft(cartID string) (Draft, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.drafts[cartID]
	return d, ok
}

// PlaceAndPay places the cart's draft and charges the (test) payment token.
// The model never sees or chooses the payment method: placement is plain Go,
// run only after a human confirmed the draft. wantHash is the Draft.Hash the
// human confirmed; if the stored draft differs (swapped after confirmation)
// nothing is placed.
//
// key makes the call idempotent: a repeat with a key that already placed an
// order returns that order and charges nothing, which is what lets a graph
// node that crashed after paying but before it was committed run again
// (graph.IdempotencyKey). An empty key disables the check.
func (s *Shop) PlaceAndPay(cartID, wantHash, paymentToken, key string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.byKey[key]; ok && key != "" {
		s.log("place-replayed")
		return o, nil
	}
	defer s.save()
	d, ok := s.drafts[cartID]
	if !ok {
		return Order{}, fmt.Errorf("no draft for cart %q", cartID)
	}
	if d.Hash() != wantHash {
		s.log("place-refused")
		return Order{}, ErrDraftChanged
	}
	s.log("place")
	o := Order{ID: fmt.Sprintf("ORD-%04d", len(s.orders)+1), Draft: d}
	if s.declined || !strings.HasPrefix(paymentToken, "tok_test_") {
		s.declined = false
		o.Status = "payment_failed"
		s.orders = append(s.orders, o)
		return o, errors.New("payment declined")
	}
	o.Status = "paid"
	o.Charge = fmt.Sprintf("ch_%s_%d", o.ID, d.TotalKop)
	s.orders = append(s.orders, o)
	delete(s.drafts, cartID)
	if key != "" {
		s.byKey[key] = o
	}
	return o, nil
}

// Orders returns placed orders.
func (s *Shop) Orders() []Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Order(nil), s.orders...)
}

// UAH formats kopecks.
func UAH(kop int) string { return fmt.Sprintf("%d.%02d UAH", kop/100, kop%100) }

// Summary is the human-readable confirmation text.
func (d Draft) Summary(s *Shop) string {
	var b strings.Builder
	for _, l := range d.Lines {
		name := l.ItemID
		if m, ok := s.menu[l.ItemID]; ok {
			name = m.Name
		}
		fmt.Fprintf(&b, "  %d × %s (%s)\n", l.Qty, name, l.Size)
	}
	fmt.Fprintf(&b, "  delivery to %s: %s, ~%d min\n", d.Address, UAH(d.DeliveryKop), d.ETAMinutes)
	if d.Notes != "" {
		fmt.Fprintf(&b, "  notes: %s\n", d.Notes)
	}
	fmt.Fprintf(&b, "  TOTAL %s", UAH(d.TotalKop))
	return b.String()
}
