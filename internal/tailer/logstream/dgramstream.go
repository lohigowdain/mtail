// Copyright 2020 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

package logstream

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/google/mtail/internal/logline"
	"github.com/google/mtail/internal/waker"
)

type dgramStream struct {
	streamBase

	cancel context.CancelFunc

	scheme  string // Datagram scheme, either "unixgram" or "udp".
	address string // Given name for the underlying socket path on the filesystem or hostport.
}

func newDgramStream(ctx context.Context, wg *sync.WaitGroup, waker waker.Waker, scheme, address string, oneShot OneShotMode) (LogStream, error) {
	if address == "" {
		return nil, ErrEmptySocketAddress
	}
	ctx, cancel := context.WithCancel(ctx)
	ss := &dgramStream{
		cancel:  cancel,
		scheme:  scheme,
		address: address,
		streamBase: streamBase{
			sourcename: fmt.Sprintf("%s://%s", scheme, address),
			lines:      make(chan *logline.LogLine),
		},
	}
	if err := ss.stream(ctx, wg, waker, oneShot); err != nil {
		return nil, err
	}
	return ss, nil
}

// The read buffer size for datagrams.
const datagramReadBufferSize = 131072

func (ds *dgramStream) stream(ctx context.Context, wg *sync.WaitGroup, waker waker.Waker, oneShot OneShotMode) error {
	c, err := net.ListenPacket(ds.scheme, ds.address)
	if err != nil {
		logErrors.Add(ds.address, 1)
		return err
	}
	glog.V(2).Infof("stream(%s): opened new datagram socket %v", ds.sourcename, c)
	b := make([]byte, datagramReadBufferSize)
	partial := bytes.NewBufferString("")
	var total int
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			glog.V(2).Infof("stream(%s): read total %d bytes", ds.sourcename, total)
			glog.V(2).Infof("stream(%s): closing connection", ds.sourcename)
			err := c.Close()
			if err != nil {
				logErrors.Add(ds.address, 1)
				glog.Info(err)
			}
			logCloses.Add(ds.address, 1)
			close(ds.lines)
			ds.cancel()
		}()
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		SetReadDeadlineOnDone(ctx, c)

		for {
			n, _, err := c.ReadFrom(b)
			glog.V(2).Infof("stream(%s): read %d bytes, err is %v", ds.sourcename, n, err)

			if ds.staleTimer != nil {
				ds.staleTimer.Stop()
			}

			// This is a test-only trick that says if we've already put this
			// logstream in graceful shutdown, then a zero-byte read is
			// equivalent to an "EOF" in connection and file oriented streams.
			if n == 0 {
				if oneShot {
					glog.V(2).Infof("stream(%s): exiting because zero byte read and one shot", ds.sourcename)
					if partial.Len() > 0 {
						ds.sendLine(ctx, partial)
					}
					return
				}
				select {
				case <-ctx.Done():
					glog.V(2).Infof("stream(%s): exiting because zero byte read after cancellation", ds.sourcename)
					if partial.Len() > 0 {
						ds.sendLine(ctx, partial)
					}
					return
				default:
				}
			}

			if n > 0 {
				total += n
				//nolint:contextcheck
				ds.decodeAndSend(ctx, n, b[:n], partial)
				ds.staleTimer = time.AfterFunc(time.Hour*24, ds.cancel)

				// No error implies more to read, so restart the loop.
				if err == nil && ctx.Err() == nil {
					continue
				}
			}

			if IsExitableError(err) {
				if partial.Len() > 0 {
					ds.sendLine(ctx, partial)
				}
				glog.V(2).Infof("stream(%s): exiting, stream has error %s", ds.sourcename, err)
				return
			}

			// Yield and wait
			glog.V(2).Infof("stream(%s): waiting", ds.sourcename)
			select {
			case <-ctx.Done():
				// Exit after next read attempt.
				// We may have started waiting here when the stop signal
				// arrives, but since that wait the file may have been
				// written to.  The file is not technically yet at EOF so
				// we need to go back and try one more read.  We'll exit
				// the stream in the zero byte handler above.
				glog.V(2).Infof("stream(%s): context cancelled, exiting after next zero byte read", ds.scheme, ds.address)
			case <-waker.Wake():
				// sleep until next Wake()
				glog.V(2).Infof("stream(%s): Wake received", ds.sourcename)
			}
		}
	}()
	return nil
}
