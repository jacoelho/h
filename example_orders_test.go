package h_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/htest"
	"github.com/jacoelho/h/problem"
)

// The orders vertical: a domain that knows nothing of HTTP, and a
// registration that owns its routes and its error translation.

type OrderID string

func ParseOrderID(s string) (OrderID, error) {
	if !strings.HasPrefix(s, "o-") {
		return "", fmt.Errorf("order id %q lacks the o- prefix", s)
	}
	return OrderID(s), nil
}

type OrderStatus string

const (
	Open   OrderStatus = "open"
	Closed OrderStatus = "closed"
)

type Order struct {
	ID     OrderID     `json:"id"`
	Item   string      `json:"item"`
	Status OrderStatus `json:"status"`
}

type NewOrder struct {
	Item string `json:"item"`
}

var (
	ErrOrderNotFound = errors.New("order not found")
	ErrOutOfStock    = errors.New("out of stock")
)

type OrderService struct {
	mu     sync.Mutex
	orders []Order
}

func (s *OrderService) Create(_ context.Context, n NewOrder) (Order, error) {
	if n.Item == "unobtainium" {
		return Order{}, fmt.Errorf("reserving %s: %w", n.Item, ErrOutOfStock)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o := Order{ID: OrderID(fmt.Sprintf("o-%d", len(s.orders)+1)), Item: n.Item, Status: Open}
	s.orders = append(s.orders, o)
	return o, nil
}

func (s *OrderService) Get(_ context.Context, id OrderID) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.orders, func(o Order) bool { return o.ID == id })
	if i < 0 {
		return Order{}, fmt.Errorf("order %s: %w", id, ErrOrderNotFound)
	}
	return s.orders[i], nil
}

func (s *OrderService) List(_ context.Context, statuses []OrderStatus) ([]Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Order
	for _, o := range s.orders {
		if len(statuses) == 0 || slices.Contains(statuses, o.Status) {
			out = append(out, o)
		}
	}
	return out, nil
}

var orderErrors = problem.Map(
	problem.Is(ErrOrderNotFound, problem.Problem{Status: http.StatusNotFound, Type: "https://example.com/probs/order-not-found", Title: "Order not found"}),
	problem.Is(ErrOutOfStock, problem.Problem{Status: http.StatusConflict, Type: "https://example.com/probs/out-of-stock", Title: "Out of stock"}),
)

func RegisterOrders(r *h.Router, svc *OrderService) {
	g := r.Group("/orders", h.Errors(orderErrors))
	g.Post("/", func(ctx context.Context, req h.Request[NewOrder]) (h.Response[Order], error) {
		o, err := svc.Create(ctx, req.Body())
		if err != nil {
			return h.Response[Order]{}, err
		}
		return h.Created(req.Link("orders", string(o.ID)), o), nil
	})
	g.Get("/{id}", h.Bind(func(p *h.Params, _ h.Empty) OrderID {
		return p.Path("id", ParseOrderID)
	}, svc.Get))
	g.Get("/", func(ctx context.Context, req h.Request[h.Empty]) (h.Response[[]Order], error) {
		var statuses []OrderStatus
		if err := req.Parse(func(p *h.Params) {
			statuses = p.QueryAll("status", h.OneOf(Open, Closed))
		}); err != nil {
			return h.Response[[]Order]{}, err
		}
		orders, err := svc.List(ctx, statuses)
		return h.OK(orders), err
	})
}

func TestOrderErrorsAreValid(t *testing.T) {
	if err := orderErrors.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestOrdersVertical(t *testing.T) {
	r := newRouter(t, h.Config{BasePath: "/shop"})
	RegisterOrders(r, &OrderService{})
	c := htest.New(t, r)

	resp := c.Do("POST", "/orders", NewOrder{Item: "tea"}).Expect(t, http.StatusCreated)
	if loc := resp.Header().Get("Location"); loc != "/shop/orders/o-1" {
		t.Fatalf("Location %q", loc)
	}
	want := Order{ID: "o-1", Item: "tea", Status: Open}
	if got := resp.JSON[Order](t); got != want {
		t.Fatalf("created %+v, want %+v", got, want)
	}

	if got := c.Do("GET", "/orders/o-1", nil).Expect(t, http.StatusOK).JSON[Order](t); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	p := c.Do("GET", "/orders/o-9", nil).Expect(t, http.StatusNotFound).Problem(t)
	if p.Type != "https://example.com/probs/order-not-found" || p.Title != "Order not found" {
		t.Fatalf("problem %+v", p)
	}

	c.Do("GET", "/orders/not-an-id", nil).Expect(t, http.StatusNotFound)
	c.Do("POST", "/orders", NewOrder{Item: "unobtainium"}).Expect(t, http.StatusConflict)

	list := c.Do("GET", "/orders?status=open&status=closed", nil).Expect(t, http.StatusOK).JSON[[]Order](t)
	if len(list) != 1 || list[0] != want {
		t.Fatalf("list %+v", list)
	}

	resp = c.Do("GET", "/orders?status=lost", nil).Expect(t, http.StatusBadRequest)
	wantJSON(t, resp.Body.Bytes(), `{"type":"about:blank","title":"Bad Request","status":400,"invalid_params":[{"in":"query","name":"status","reason":"invalid"}]}`)
}
