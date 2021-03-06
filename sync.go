package beehive

import (
	"encoding/gob"
	"errors"
	"math/rand"
	"sync"

	"github.com/kandoo/beehive/Godeps/_workspace/src/golang.org/x/net/context"

	bhgob "github.com/kandoo/beehive/gob"
)

// request represents a generic sync request.
type request struct {
	ID   uint64
	Data interface{} // Data of the request. Must be registered in gob.
}

// Type returns the type of this request. It is unique for each data type.
func (r request) Type() string {
	if r.Data == nil {
		return "request"
	}
	return "request-" + MsgType(r.Data)
}

// response represents a generic sync reponse.
type response struct {
	ID   uint64      // ID is the request ID.
	Data interface{} // Data of the response. Must be registered in gob.
	Err  error       // Err is error, if any.
}

// Type returns the type of this response. It is unique for each data type.
func (r response) Type() string {
	if r.Data == nil {
		return "response"
	}
	return "response-" + MsgType(r.Data)
}

type requestAndChan struct {
	req request
	ch  chan response
}

// Sync is a generic DetachedHandler for sync request processing, and also
// provides Handle, HandleFunc, and Process for the clients.
type Sync struct {
	app *app

	reqch chan requestAndChan
	done  chan chan struct{}

	m    sync.Mutex
	reqs map[uint64]chan response
}

// NewSync creates a Sync for the application.
// This method should be called only once for each application.
func NewSync(a App) *Sync {
	s := &Sync{
		app:   a.(*app),
		reqch: make(chan requestAndChan, 2048),
		done:  make(chan chan struct{}),
		reqs:  make(map[uint64]chan response),
	}
	a.Detached(s)
	return s
}

// Process processes a request and returns the response and error.
func (h *Sync) Process(ctx context.Context, req interface{}) (res interface{},
	err error) {

	id := uint64(rand.Int63())
	ch := make(chan response, 1)
	h.reqch <- requestAndChan{
		req: request{ID: id, Data: req},
		ch:  ch,
	}
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, errors.New(r.Err.Error())
		}
		return r.Data, nil

	case <-ctx.Done():
		// TODO(soheil): what if the request is not enqued yet?
		h.deque(id)
		return nil, ctx.Err()
	}
}

// Start is to implement DetachedHandler.
func (h *Sync) Start(ctx RcvContext) {
	for {
		select {
		case ch := <-h.done:
			h.drain()
			ch <- struct{}{}
		case rnc := <-h.reqch:
			h.enque(rnc.req.ID, rnc.ch)
			ctx.Emit(rnc.req)
		}
	}
}

var (
	// ErrSyncStopped returned when the sync handler is stopped before receiving
	// the response.
	ErrSyncStopped = bhgob.Error("sync: stopped")
	// ErrSyncNoSuchRequest returned when we cannot find the request for that
	// response.
	ErrSyncNoSuchRequest = bhgob.Error("sync: request not found")
	// ErrSyncDuplicateResponse returned when there is a duplicate repsonse to the
	// sync request.
	ErrSyncDuplicateResponse = bhgob.Error("sync: duplicate response")
)

// Stop is to implement DetachedHandler.
func (h *Sync) Stop(ctx RcvContext) {
	ack := make(chan struct{})
	h.done <- ack
	<-ack
}

// Rcv is to implement DetachedHandler.
func (h *Sync) Rcv(msg Msg, ctx RcvContext) error {
	res := msg.Data().(response)
	ch, err := h.deque(res.ID)
	if err != nil {
		return err
	}
	ch <- res
	return nil
}

func (h *Sync) drain() {
	h.m.Lock()
	for id, ch := range h.reqs {
		ch <- response{
			ID:  id,
			Err: ErrSyncStopped,
		}
		delete(h.reqs, id)
	}
	h.m.Unlock()
}

func (h *Sync) enque(id uint64, ch chan response) {
	h.m.Lock()
	h.reqs[id] = ch
	h.m.Unlock()
}

func (h *Sync) deque(id uint64) (chan response, error) {
	h.m.Lock()
	ch, ok := h.reqs[id]
	h.m.Unlock()
	if !ok {
		return nil, ErrSyncNoSuchRequest
	}
	delete(h.reqs, id)
	return ch, nil
}

type syncRcvContext struct {
	RcvContext
	id      uint64
	from    uint64
	replied bool
}

func (ctx *syncRcvContext) ReplyTo(msg Msg, replyData interface{}) error {
	if msg.From() != ctx.from {
		return ctx.RcvContext.ReplyTo(msg, replyData)
	}

	if ctx.replied {
		return ErrSyncDuplicateResponse
	}

	ctx.replied = true
	r := response{
		ID:   ctx.id,
		Data: replyData,
	}
	return ctx.RcvContext.ReplyTo(msg, r)
}

type syncHandler struct {
	handler Handler
}

func (h syncHandler) Rcv(m Msg, ctx RcvContext) error {
	req := m.Data().(request)
	sm := msg{
		MsgData: req.Data,
		MsgFrom: m.From(),
		MsgTo:   m.To(),
	}
	sc := syncRcvContext{
		RcvContext: ctx,
		id:         req.ID,
		from:       m.From(),
	}
	err := h.handler.Rcv(sm, &sc)
	if err != nil {
		ctx.AbortTx()
		r := response{
			ID:  req.ID,
			Err: bhgob.Error(err.Error()),
		}
		ctx.ReplyTo(m, r)
		return err
	}
	if !sc.replied {
		r := response{
			ID: req.ID,
		}
		ctx.ReplyTo(m, r)
	}
	return nil
}

func (h syncHandler) Map(m Msg, ctx MapContext) MappedCells {
	s := msg{
		MsgData: m.Data().(request).Data,
		MsgFrom: m.From(),
		MsgTo:   m.To(),
	}
	return h.handler.Map(s, ctx)
}

// Handle wraps h as a handler that can handle sync requests and install it on
// the application.
func (s *Sync) Handle(msg interface{}, h Handler) {
	s.app.hive.RegisterMsg(msg)
	req := request{Data: msg}
	s.app.Handle(req, syncHandler{handler: h})
}

// HandleFunc wraps the map and rcv functions as a handlers that can handle
// sync requests and install it on the application.
func (s *Sync) HandleFunc(msg interface{}, m MapFunc, r RcvFunc) {
	s.Handle(msg, &funcHandler{mapFunc: m, rcvFunc: r})
}

func init() {
	gob.Register(request{})
	gob.Register(response{})
}

var _ DetachedHandler = &Sync{}
var _ Handler = syncHandler{}
