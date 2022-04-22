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

package xep0363

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	kitlog "github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/jackal-xmpp/stravaganza"
	"github.com/ortuman/jackal/pkg/cluster/resourcemanager"
	"github.com/ortuman/jackal/pkg/hook"
	"github.com/ortuman/jackal/pkg/host"
	"github.com/ortuman/jackal/pkg/router"
	xmpputil "github.com/ortuman/jackal/pkg/util/xmpp"
)

const (
	// ModuleName represents http_upload module name.
	ModuleName = "http_upload"
	// XEPNumber represents carbons XEP number.
	XEPNumber = "0363"
	namespace = "urn:xmpp:http:upload:0"
)

// HttpUpload represents carbons (XEP-0363) module type.
type HttpUpload struct {
	cfg    Config
	hosts  hosts
	router router.Router
	resMng resourcemanager.Manager
	hk     *hook.Hooks
	logger kitlog.Logger
}

type Config struct {
	BaseUrl string `fig:"base_url"`
	Secret  string `fig:"secret"`
}

// New returns a new initialized HttpUpload instance.
func New(
	cfg Config,
	router router.Router,
	hosts *host.Hosts,
	resMng resourcemanager.Manager,
	hk *hook.Hooks,
	logger kitlog.Logger,
) *HttpUpload {
	return &HttpUpload{
		cfg:    cfg,
		hosts:  hosts,
		router: router,
		resMng: resMng,
		hk:     hk,
		logger: kitlog.With(logger, "module", ModuleName, "xep", XEPNumber),
	}
}

// Name returns specific module name.
func (m *HttpUpload) Name() string {
	return ModuleName
}

// StreamFeature returns module stream feature element.
func (m *HttpUpload) StreamFeature(ctx context.Context, domain string) (stravaganza.Element, error) {
	return nil, nil
}

// ServerFeatures returns module server features.
func (m *HttpUpload) ServerFeatures(ctx context.Context) ([]string, error) {
	return []string{namespace}, nil
}

// AccountFeatures returns module account features.
func (m *HttpUpload) AccountFeatures(ctx context.Context) ([]string, error) {
	return nil, nil
}

// ProcessIQ process a time iq.
func (m *HttpUpload) ProcessIQ(ctx context.Context, iq *stravaganza.IQ) error {
	fmt.Printf("IQ: %+v\n", iq)
	return nil
}

// Start starts module.
func (m *HttpUpload) Start(ctx context.Context) error {
	m.hk.AddHook(hook.C2SStreamIQReceived, m.onC2SIQRecv, hook.DefaultPriority)

	level.Info(m.logger).Log("msg", "started http_upload module")
	return nil
}

// Stop stops module.
func (m *HttpUpload) Stop(ctx context.Context) error {
	level.Info(m.logger).Log("msg", "stopped http_upload module")
	return nil
}

func (m *HttpUpload) onC2SIQRecv(ctx context.Context, execCtx *hook.ExecutionContext) error {
	inf := execCtx.Info.(*hook.C2SStreamInfo)
	iq := inf.Element.(*stravaganza.IQ)

	request := iq.Child("request")
	if request == nil {
		// not for this module
		return nil
	}

	fileName := request.Attribute("filename")
	if fileName == "" {
		// required by xep0363
		return nil
	}
	length := request.Attribute("size")
	if length == "" {
		// required by xep0363
		return nil
	}

	mac := hmac.New(sha256.New, []byte(m.cfg.Secret))
	mac.Write([]byte(fileName + " " + length))
	macString := hex.EncodeToString(mac.Sum(nil))

	sb := stravaganza.NewBuilder("slot").
		WithAttribute(stravaganza.Namespace, namespace).
		WithChild(
			stravaganza.NewBuilder("put").
				WithAttribute("url", fmt.Sprintf("%s/%s?v=%s", m.cfg.BaseUrl, fileName, macString)).
				Build(),
		).
		WithChild(
			stravaganza.NewBuilder("get").
				WithAttribute("url", fmt.Sprintf("%s/%s", m.cfg.BaseUrl, fileName)).
				Build(),
		).
		Build()
	_, _ = m.router.Route(ctx, xmpputil.MakeResultIQ(iq, sb))

	return nil
}
