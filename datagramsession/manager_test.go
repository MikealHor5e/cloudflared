package datagramsession

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

func TestManagerServe(t *testing.T) {
	const (
		sessions = 20
		msgs     = 50
	)
	log := zerolog.Nop()
	transport := &mockQUICTransport{
		reqChan:  newDatagramChannel(),
		respChan: newDatagramChannel(),
	}
	mg := NewManager(transport, &log)

	eyeballTracker := make(map[uuid.UUID]*datagramChannel)
	for i := 0; i < sessions; i++ {
		sessionID := uuid.New()
		eyeballTracker[sessionID] = newDatagramChannel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan struct{})
	go func(ctx context.Context) {
		mg.Serve(ctx)
		close(serveDone)
	}(ctx)

	go func(ctx context.Context) {
		for {
			sessionID, payload, err := transport.respChan.Receive(ctx)
			if err != nil {
				require.Equal(t, context.Canceled, err)
				return
			}
			respChan := eyeballTracker[sessionID]
			require.NoError(t, respChan.Send(ctx, sessionID, payload))
		}
	}(ctx)

	errGroup, ctx := errgroup.WithContext(ctx)
	for sID, receiver := range eyeballTracker {
		// Assign loop variables to local variables
		sessionID := sID
		eyeballRespReceiver := receiver
		errGroup.Go(func() error {
			payload := testPayload(sessionID)
			expectResp := testResponse(payload)

			cfdConn, originConn := net.Pipe()

			origin := mockOrigin{
				expectMsgCount: msgs,
				expectedMsg:    payload,
				expectedResp:   expectResp,
				conn:           originConn,
			}
			eyeball := mockEyeball{
				expectMsgCount:  msgs,
				expectedMsg:     expectResp,
				expectSessionID: sessionID,
				respReceiver:    eyeballRespReceiver,
			}

			reqErrGroup, reqCtx := errgroup.WithContext(ctx)
			reqErrGroup.Go(func() error {
				return origin.serve()
			})
			reqErrGroup.Go(func() error {
				return eyeball.serve(reqCtx)
			})

			session, err := mg.RegisterSession(ctx, sessionID, cfdConn)
			require.NoError(t, err)

			sessionDone := make(chan struct{})
			go func() {
				session.Serve(ctx)
				close(sessionDone)
			}()

			for i := 0; i < msgs; i++ {
				require.NoError(t, transport.newRequest(ctx, sessionID, testPayload(sessionID)))
			}

			// Make sure eyeball and origin have received all messages before unregistering the session
			require.NoError(t, reqErrGroup.Wait())

			require.NoError(t, mg.UnregisterSession(ctx, sessionID))
			<-sessionDone

			return nil
		})
	}

	require.NoError(t, errGroup.Wait())
	cancel()
	transport.close()
	<-serveDone
}

type mockOrigin struct {
	expectMsgCount int
	expectedMsg    []byte
	expectedResp   []byte
	conn           io.ReadWriteCloser
}

func (mo *mockOrigin) serve() error {
	expectedMsgLen := len(mo.expectedMsg)
	readBuffer := make([]byte, expectedMsgLen+1)
	for i := 0; i < mo.expectMsgCount; i++ {
		n, err := mo.conn.Read(readBuffer)
		if err != nil {
			return err
		}
		if n != expectedMsgLen {
			return fmt.Errorf("Expect to read %d bytes, read %d", expectedMsgLen, n)
		}
		if !bytes.Equal(readBuffer[:n], mo.expectedMsg) {
			return fmt.Errorf("Expect %v, read %v", mo.expectedMsg, readBuffer[:n])
		}

		_, err = mo.conn.Write(mo.expectedResp)
		if err != nil {
			return err
		}
	}
	return nil
}

func testPayload(sessionID uuid.UUID) []byte {
	return []byte(fmt.Sprintf("Message from %s", sessionID))
}

func testResponse(msg []byte) []byte {
	return []byte(fmt.Sprintf("Response to %v", msg))
}

type mockEyeball struct {
	expectMsgCount  int
	expectedMsg     []byte
	expectSessionID uuid.UUID
	respReceiver    *datagramChannel
}

func (me *mockEyeball) serve(ctx context.Context) error {
	for i := 0; i < me.expectMsgCount; i++ {
		sessionID, msg, err := me.respReceiver.Receive(ctx)
		if err != nil {
			return err
		}
		if sessionID != me.expectSessionID {
			return fmt.Errorf("Expect session %s, got %s", me.expectSessionID, sessionID)
		}
		if !bytes.Equal(msg, me.expectedMsg) {
			return fmt.Errorf("Expect %v, read %v", me.expectedMsg, msg)
		}
	}
	return nil
}

// datagramChannel is a channel for Datagram with wrapper to send/receive with context
type datagramChannel struct {
	datagramChan chan *newDatagram
	closedChan   chan struct{}
}

func newDatagramChannel() *datagramChannel {
	return &datagramChannel{
		datagramChan: make(chan *newDatagram, 1),
		closedChan:   make(chan struct{}),
	}
}

func (rc *datagramChannel) Send(ctx context.Context, sessionID uuid.UUID, payload []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-rc.closedChan:
		return fmt.Errorf("datagram channel closed")
	case rc.datagramChan <- &newDatagram{sessionID: sessionID, payload: payload}:
		return nil
	}
}

func (rc *datagramChannel) Receive(ctx context.Context) (uuid.UUID, []byte, error) {
	select {
	case <-ctx.Done():
		return uuid.Nil, nil, ctx.Err()
	case <-rc.closedChan:
		return uuid.Nil, nil, fmt.Errorf("datagram channel closed")
	case msg := <-rc.datagramChan:
		return msg.sessionID, msg.payload, nil
	}
}

func (rc *datagramChannel) Close() {
	// No need to close msgChan, it will be garbage collect once there is no reference to it
	close(rc.closedChan)
}