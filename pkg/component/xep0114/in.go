// Copyright 2022 The jackal Authors
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

package xep0114

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	kitlog "github.com/go-kit/log"

	"github.com/go-kit/log/level"

	"github.com/jackal-xmpp/runqueue/v2"
	"github.com/jackal-xmpp/stravaganza"
	streamerror "github.com/jackal-xmpp/stravaganza/errors/stream"
	"github.com/jackal-xmpp/stravaganza/jid"
	"github.com/ortuman/jackal/pkg/component"
	"github.com/ortuman/jackal/pkg/component/extcomponentmanager"
	"github.com/ortuman/jackal/pkg/hook"
	"github.com/ortuman/jackal/pkg/host"
	xmppparser "github.com/ortuman/jackal/pkg/parser"
	"github.com/ortuman/jackal/pkg/router"
	xmppsession "github.com/ortuman/jackal/pkg/session"
	"github.com/ortuman/jackal/pkg/shaper"
	"github.com/ortuman/jackal/pkg/transport"
)

type inComponentID uint64

func (i inComponentID) String() string {
	return fmt.Sprintf("ext_comp:%d", i)
}

type inComponentState uint32

const (
	connecting inComponentState = iota
	handshaking
	authenticated
	disconnected
)

var disconnectTimeout = time.Second * 5

type inConfig struct {
	reqTimeout    time.Duration
	maxStanzaSize int
	secret        string
}

type inComponent struct {
	id           inComponentID
	cfg          inConfig
	tr           transport.Transport
	shapers      shaper.Shapers
	session      session
	comps        components
	router       router.Router
	extCompMng   externalComponentManager
	inHub        *inHub
	hk           *hook.Hooks
	logger       kitlog.Logger
	rq           *runqueue.RunQueue
	discTm       *time.Timer
	doneCh       chan struct{}
	sendDisabled bool

	mu       sync.RWMutex
	ctx      context.Context
	cancelFn context.CancelFunc
	jd       jid.JID
	state    uint32
}

func newInComponent(
	tr transport.Transport,
	hosts *host.Hosts,
	comps *component.Components,
	extCompMng *extcomponentmanager.Manager,
	stmHub *inHub,
	router router.Router,
	shapers shaper.Shapers,
	hk *hook.Hooks,
	logger kitlog.Logger,
	cfg inConfig,
) (*inComponent, error) {
	// set default rate limiter
	rLim := shapers.DefaultS2S().RateLimiter()
	if err := tr.SetReadRateLimiter(rLim); err != nil {
		return nil, err
	}
	// create session
	id := nextStreamID()

	sLogger := kitlog.With(logger, "id", id)
	session := xmppsession.New(
		xmppsession.ComponentSession,
		id.String(),
		tr,
		hosts,
		xmppsession.Config{
			MaxStanzaSize: cfg.maxStanzaSize,
		},
		sLogger,
	)
	// init stream
	ctx, cancelFn := context.WithCancel(context.Background())
	return &inComponent{
		id:         id,
		cfg:        cfg,
		tr:         tr,
		session:    session,
		comps:      comps,
		router:     router,
		inHub:      stmHub,
		extCompMng: extCompMng,
		ctx:        ctx,
		cancelFn:   cancelFn,
		rq:         runqueue.New(id.String()),
		doneCh:     make(chan struct{}),
		shapers:    shapers,
		hk:         hk,
		logger:     sLogger,
	}, nil
}

func (s *inComponent) start() error {
	s.inHub.register(s)
	level.Info(s.logger).Log("msg", "registered external component stream")

	ctx, cancel := s.requestContext()
	_, err := s.runHook(ctx, hook.ExternalComponentRegistered, &hook.ExternalComponentInfo{
		ID: s.id.String(),
	})
	cancel()

	if err != nil {
		return err
	}
	reportConnectionRegistered()

	s.readLoop()
	return nil
}

func (s *inComponent) sendStanza(stanza stravaganza.Stanza) <-chan error {
	errCh := make(chan error, 1)
	s.rq.Run(func() {
		ctx, cancel := s.requestContext()
		defer cancel()
		errCh <- s.sendElement(ctx, stanza)
	})
	return errCh
}

func (s *inComponent) shutdown() <-chan error {
	errCh := make(chan error, 1)
	s.rq.Run(func() {
		ctx, cancel := s.requestContext()
		defer cancel()
		errCh <- s.disconnect(ctx, streamerror.E(streamerror.SystemShutdown))
	})
	return errCh
}

func (s *inComponent) done() <-chan struct{} {
	return s.doneCh
}

func (s *inComponent) readLoop() {
	s.restartSession()

	s.tr.SetConnectDeadlineHandler(s.connTimeout)
	s.tr.SetKeepAliveDeadlineHandler(s.connTimeout)

	elem, sErr := s.session.Receive()
	for {
		if s.getState() == disconnected {
			return
		}
		s.handleSessionResult(elem, sErr)
		elem, sErr = s.session.Receive()
	}
}

func (s *inComponent) connTimeout() {
	s.rq.Run(func() {
		ctx, cancel := s.requestContext()
		defer cancel()
		_ = s.disconnect(ctx, streamerror.E(streamerror.ConnectionTimeout))
	})
}

func (s *inComponent) handleSessionResult(elem stravaganza.Element, sErr error) {
	doneCh := make(chan struct{})
	s.rq.Run(func() {
		defer close(doneCh)

		ctx, cancel := s.requestContext()
		defer cancel()

		switch {
		case sErr != nil:
			s.handleSessionError(ctx, sErr)
		case sErr == nil && elem != nil:
			err := s.handleElement(ctx, elem)
			if err != nil {
				level.Warn(s.logger).Log("msg", "failed to process incoming component session element", "err", err)
				return
			}
		}
	})
	<-doneCh
}

func (s *inComponent) handleElement(ctx context.Context, elem stravaganza.Element) error {
	// run received element hook
	hInf := &hook.ExternalComponentInfo{
		ID:      s.id.String(),
		Host:    s.getJID().Domain(),
		Element: elem,
	}
	halted, err := s.runHook(ctx, hook.ExternalComponentElementReceived, hInf)
	if err != nil {
		return err
	}
	if halted {
		return nil
	}

	t0 := time.Now()
	switch s.getState() {
	case connecting:
		return s.handleConnecting(ctx, hInf.Element)
	case handshaking:
		return s.handleHandshaking(ctx, hInf.Element)
	case authenticated:
		return s.handleAuthenticated(ctx, hInf.Element)
	}
	reportIncomingRequest(
		elem.Name(),
		elem.Attribute(stravaganza.Type),
		time.Since(t0).Seconds(),
	)
	return nil
}

func (s *inComponent) handleConnecting(ctx context.Context, elem stravaganza.Element) error {
	cHost := elem.Attribute(stravaganza.To)
	if len(cHost) == 0 {
		return s.disconnect(ctx, streamerror.E(streamerror.HostUnknown))
	}
	if s.comps.IsComponentHost(cHost) {
		return s.disconnect(ctx, streamerror.E(streamerror.Conflict))
	}
	// set component host JID
	j, _ := jid.New("", cHost, "", true)
	s.setJID(j)
	s.session.SetFromJID(j)

	if err := s.updateTransportRateLimiter(); err != nil {
		return err
	}
	s.setState(handshaking)
	_ = s.session.OpenComponent(ctx)
	return nil
}

func (s *inComponent) handleHandshaking(ctx context.Context, elem stravaganza.Element) error {
	if elem.Name() != "handshake" {
		return s.disconnect(ctx, streamerror.E(streamerror.UnsupportedStanzaType))
	}
	// compute handshake
	h := sha1.New()
	h.Write([]byte(s.session.StreamID() + s.cfg.secret))
	hs := hex.EncodeToString(h.Sum(nil))

	if elem.Text() != hs {
		return s.disconnect(ctx, streamerror.E(streamerror.NotAuthorized))
	}

	if err := s.registerComponent(ctx); err != nil {
		return err
	}
	s.setState(authenticated)
	return s.sendElement(ctx, stravaganza.NewBuilder("handshake").Build())
}

func (s *inComponent) handleAuthenticated(ctx context.Context, elem stravaganza.Element) error {
	switch stanza := elem.(type) {
	case stravaganza.Stanza:
		_, _ = s.router.Route(ctx, stanza)
		return nil

	default:
		return s.disconnect(ctx, streamerror.E(streamerror.UnsupportedStanzaType))
	}
}

func (s *inComponent) handleSessionError(ctx context.Context, err error) {
	switch err {
	case xmppparser.ErrStreamClosedByPeer:
		_ = s.session.Close(ctx)
		fallthrough

	default:
		_ = s.close(ctx)
	}
}

func (s *inComponent) disconnect(ctx context.Context, streamErr *streamerror.Error) error {
	if s.getState() == connecting {
		_ = s.session.OpenComponent(ctx)
	}
	if streamErr != nil {
		if err := s.sendElement(ctx, streamErr.Element()); err != nil {
			return err
		}
	}
	// close stream session and wait for the other entity to close its stream
	_ = s.session.Close(ctx)

	if s.getState() != connecting && streamErr != nil && streamErr.Reason == streamerror.ConnectionTimeout {
		s.discTm = time.AfterFunc(disconnectTimeout, func() {
			s.rq.Run(func() {
				ctx, cancel := s.requestContext()
				defer cancel()
				_ = s.close(ctx)
			})
		})
		s.sendDisabled = true // avoid sending anymore stanzas while closing
		return nil
	}
	return s.close(ctx)
}

func (s *inComponent) close(ctx context.Context) error {
	if s.getState() == disconnected {
		return nil // already disconnected
	}
	defer close(s.doneCh)

	s.setState(disconnected)

	var cHost string
	if s.getState() == authenticated {
		// unregister component
		if err := s.unregisterComponent(ctx); err != nil {
			return err
		}
		cHost = s.getJID().String()
	}
	s.inHub.unregister(s)
	level.Info(s.logger).Log("msg", "unregistered external component stream")

	_, err := s.runHook(ctx, hook.ExternalComponentUnregistered, &hook.ExternalComponentInfo{
		ID:   s.id.String(),
		Host: cHost,
	})
	if err != nil {
		return err
	}
	reportConnectionUnregistered()

	// close underlying transport
	_ = s.tr.Close()
	return nil
}

func (s *inComponent) restartSession() {
	_ = s.session.Reset(s.tr)
	s.setState(connecting)
}

func (s *inComponent) sendElement(ctx context.Context, elem stravaganza.Element) error {
	if s.sendDisabled {
		return nil
	}
	err := s.session.Send(ctx, elem)
	reportOutgoingRequest(
		elem.Name(),
		elem.Attribute(stravaganza.Type),
	)
	return err
}

func (s *inComponent) registerComponent(ctx context.Context) error {
	cHost := s.getJID().Domain()
	if err := s.comps.RegisterComponent(ctx, &streamComponent{stm: s}); err != nil {
		return err
	}
	if err := s.extCompMng.RegisterComponentHost(ctx, cHost); err != nil {
		return err
	}
	level.Info(s.logger).Log("msg", "registered external component", "component_host", cHost)
	return nil
}

func (s *inComponent) unregisterComponent(ctx context.Context) error {
	cHost := s.getJID().Domain()
	if err := s.comps.UnregisterComponent(ctx, cHost); err != nil {
		return err
	}
	if err := s.extCompMng.UnregisterComponentHost(ctx, cHost); err != nil {
		return err
	}
	level.Info(s.logger).Log("msg", "unregistered external component", "component_host", cHost)
	return nil
}

func (s *inComponent) updateTransportRateLimiter() error {
	// update rate limiter
	j := s.getJID()
	rLim := s.shapers.MatchingJID(j).RateLimiter()
	return s.tr.SetReadRateLimiter(rLim)
}

func (s *inComponent) setJID(jd *jid.JID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jd = *jd
}

func (s *inComponent) getJID() *jid.JID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return &s.jd
}

func (s *inComponent) setState(state inComponentState) {
	atomic.StoreUint32(&s.state, uint32(state))
}

func (s *inComponent) getState() inComponentState {
	return inComponentState(atomic.LoadUint32(&s.state))
}

func (s *inComponent) runHook(ctx context.Context, hookName string, inf *hook.ExternalComponentInfo) (halt bool, err error) {
	return s.hk.Run(ctx, hookName, &hook.ExecutionContext{
		Info:   inf,
		Sender: s,
	})
}

func (s *inComponent) requestContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.cfg.reqTimeout)
}

var currentID uint64

func nextStreamID() inComponentID {
	return inComponentID(atomic.AddUint64(&currentID, 1))
}
