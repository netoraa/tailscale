// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !plan9

// Package sessionrecording contains functionality for recording Kubernetes API
// server proxy 'kubectl exec' sessions.
package sessionrecording

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/pkg/errors"
	"go.uber.org/zap"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/k8s-operator/sessionrecording/spdy"
	"tailscale.com/k8s-operator/sessionrecording/tsrecorder"
	"tailscale.com/sessionrecording"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/tstime"
	"tailscale.com/util/clientmetric"
	"tailscale.com/util/multierr"
)

const SPDYProtocol protocol = "SPDY"

// protocol is the streaming protocol of the hijacked session. Supported
// protocols are SPDY.
type protocol string

var (
	// CounterSessionRecordingsAttempted counts the number of session recording attempts.
	CounterSessionRecordingsAttempted = clientmetric.NewCounter("k8s_auth_proxy_session_recordings_attempted")

	// counterSessionRecordingsUploaded counts the number of successfully uploaded session recordings.
	counterSessionRecordingsUploaded = clientmetric.NewCounter("k8s_auth_proxy_session_recordings_uploaded")
)

func New(ts *tsnet.Server, req *http.Request, who *apitype.WhoIsResponse, w http.ResponseWriter, pod, ns string, proto protocol, addrs []netip.AddrPort, failOpen bool, connFunc RecorderDialFn, log *zap.SugaredLogger) *Hijacker {
	return &Hijacker{
		ts:                ts,
		req:               req,
		who:               who,
		ResponseWriter:    w,
		pod:               pod,
		ns:                ns,
		addrs:             addrs,
		failOpen:          failOpen,
		connectToRecorder: connFunc,
		proto:             proto,
		log:               log,
	}
}

// Hijacker implements [net/http.Hijacker] interface.
// It must be configured with an http request for a 'kubectl exec' session that
// needs to be recorded. It knows how to hijack the connection and configure for
// the session contents to be sent to a tsrecorder instance.
type Hijacker struct {
	http.ResponseWriter
	ts                *tsnet.Server
	req               *http.Request
	who               *apitype.WhoIsResponse
	log               *zap.SugaredLogger
	pod               string           // pod being exec-d
	ns                string           // namespace of the pod being exec-d
	addrs             []netip.AddrPort // tsrecorder addresses
	failOpen          bool             // whether to fail open if recording fails
	connectToRecorder RecorderDialFn
	proto             protocol // streaming protocol
}

// RecorderDialFn dials the specified netip.AddrPorts that should be tsrecorder
// addresses. It tries to connect to recorder endpoints one by one, till one
// connection succeeds. In case of success, returns a list with a single
// successful recording attempt and an error channel. If the connection errors
// after having been established, an error is sent down the channel.
type RecorderDialFn func(context.Context, []netip.AddrPort, func(context.Context, string, string) (net.Conn, error)) (io.WriteCloser, []*tailcfg.SSHRecordingAttempt, <-chan error, error)

// Hijack hijacks a 'kubectl exec' session and configures for the session
// contents to be sent to a recorder.
func (h *Hijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.log.Infof("recorder addrs: %v, failOpen: %v", h.addrs, h.failOpen)
	reqConn, brw, err := h.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, fmt.Errorf("error hijacking connection: %w", err)
	}

	conn, err := h.setUpRecording(context.Background(), reqConn)
	if err != nil {
		return nil, nil, fmt.Errorf("error setting up session recording: %w", err)
	}
	return conn, brw, nil
}

// setupRecording attempts to connect to the recorders set via
// spdyHijacker.addrs. Returns conn from provided opts, wrapped in recording
// logic. If connecting to the recorder fails or an error is received during the
// session and spdyHijacker.failOpen is false, connection will be closed.
func (h *Hijacker) setUpRecording(ctx context.Context, conn net.Conn) (net.Conn, error) {
	const (
		// https://docs.asciinema.org/manual/asciicast/v2/
		asciicastv2 = 2
	)
	var wc io.WriteCloser
	h.log.Infof("kubectl exec session will be recorded, recorders: %v, fail open policy: %t", h.addrs, h.failOpen)
	// TODO (irbekrm): send client a message that session will be recorded.
	rw, _, errChan, err := h.connectToRecorder(ctx, h.addrs, h.ts.Dial)
	if err != nil {
		msg := fmt.Sprintf("error connecting to session recorders: %v", err)
		if h.failOpen {
			msg = msg + "; failure mode is 'fail open'; continuing session without recording."
			h.log.Warnf(msg)
			return conn, nil
		}
		msg = msg + "; failure mode is 'fail closed'; closing connection."
		if err := closeConnWithWarning(conn, msg); err != nil {
			return nil, multierr.New(errors.New(msg), err)
		}
		return nil, errors.New(msg)
	}

	// TODO (irbekrm): log which recorder
	h.log.Info("successfully connected to a session recorder")
	wc = rw
	cl := tstime.DefaultClock{}
	rec := tsrecorder.New(wc, cl, cl.Now(), h.failOpen)
	qp := h.req.URL.Query()
	ch := sessionrecording.CastHeader{
		Version:   asciicastv2,
		Timestamp: cl.Now().Unix(),
		Command:   strings.Join(qp["command"], " "),
		SrcNode:   strings.TrimSuffix(h.who.Node.Name, "."),
		SrcNodeID: h.who.Node.StableID,
		Kubernetes: &sessionrecording.Kubernetes{
			PodName:   h.pod,
			Namespace: h.ns,
			Container: strings.Join(qp["container"], " "),
		},
	}
	if !h.who.Node.IsTagged() {
		ch.SrcNodeUser = h.who.UserProfile.LoginName
		ch.SrcNodeUserID = h.who.Node.User
	} else {
		ch.SrcNodeTags = h.who.Node.Tags
	}
	lc := spdy.New(conn, rec, ch, h.log)
	go func() {
		var err error
		select {
		case <-ctx.Done():
			return
		case err = <-errChan:
		}
		if err == nil {
			counterSessionRecordingsUploaded.Add(1)
			h.log.Info("finished uploading the recording")
			return
		}
		msg := fmt.Sprintf("connection to the session recorder errorred: %v;", err)
		if h.failOpen {
			msg += msg + "; failure mode is 'fail open'; continuing session without recording."
			h.log.Info(msg)
			return
		}
		msg += "; failure mode set to 'fail closed'; closing connection"
		h.log.Error(msg)
		lc.Fail()
		// TODO (irbekrm): write a message to the client
		if err := lc.Close(); err != nil {
			h.log.Infof("error closing recorder connections: %v", err)
		}
		return
	}()
	return lc, nil
}

func closeConnWithWarning(conn net.Conn, msg string) error {
	b := io.NopCloser(bytes.NewBuffer([]byte(msg)))
	resp := http.Response{Status: http.StatusText(http.StatusForbidden), StatusCode: http.StatusForbidden, Body: b}
	if err := resp.Write(conn); err != nil {
		return multierr.New(fmt.Errorf("error writing msg %q to conn: %v", msg, err), conn.Close())
	}
	return conn.Close()
}
