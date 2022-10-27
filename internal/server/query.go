package server

import (
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgproto3"
)

const (
	StatusUnset byte = 0
	StatusIdle  byte = 'I'
	StatusInTx  byte = 'T'
	StatusError byte = 'E'
)

type ClientConn struct {
	handle   *pgproto3.Backend
	txStatus byte
	logger   *log.Logger
	pool     *Pool

	// This is set to non-nil if there's an active transaction going on.
	serverConn *ServerConn

	schema string
}

func NewClientConn(handle *pgproto3.Backend, logger *log.Logger, pool *Pool, schema string) *ClientConn {
	return &ClientConn{
		handle: handle,
		logger: logger,
		pool:   pool,
		schema: schema,
	}
}

func (cc *ClientConn) handleQuery(feMsg pgproto3.FrontendMessage) error {
	// Leasing a connection
	if cc.serverConn == nil {
		conn, err := cc.pool.AcquireConn()
		if err != nil {
			return fmt.Errorf("error while acquiring conn: %w", err)
		}
		// TODO: exec schema search path
		cc.serverConn = conn
	}
	serverEnd := pgproto3.NewFrontend(cc.serverConn.Conn(), cc.serverConn.Conn())
	// serverEnd.Trace(cc.logger.Writer(), pgproto3.TracerOptions{})
	serverEnd.Send(feMsg)
	if err := serverEnd.Flush(); err != nil {
		return fmt.Errorf("error while flushing queryMsg: %w", err)
	}

	if err := cc.readBackendResponse(serverEnd); err != nil {
		return err
	}

	return nil
}

func (cc *ClientConn) handleExtendedQuery(feMsg pgproto3.FrontendMessage) error {
	// Leasing a connection
	if cc.serverConn == nil {
		conn, err := cc.pool.AcquireConn()
		if err != nil {
			return fmt.Errorf("error while acquiring conn: %w", err)
		}
		// TODO: exec schema search path
		cc.serverConn = conn
	}
	serverEnd := pgproto3.NewFrontend(cc.serverConn.Conn(), cc.serverConn.Conn())
	// serverEnd.Trace(cc.logger.Writer(), pgproto3.TracerOptions{})
	serverEnd.Send(feMsg)

	for {
		feMsg, err := cc.handle.Receive()
		if err != nil {
			return fmt.Errorf("error while receiving msg in extendedQuery: %w", err)
		}

		serverEnd.Send(feMsg)

		// Keep reading until we see a SYNC message
		_, ok := feMsg.(*pgproto3.Sync)
		if ok {
			break
		}
	}

	if err := serverEnd.Flush(); err != nil {
		return fmt.Errorf("error while flushing extendedQuery: %w", err)
	}

	if err := cc.readBackendResponse(serverEnd); err != nil {
		return err
	}

	return nil
}

func (cc *ClientConn) readBackendResponse(serverEnd *pgproto3.Frontend) error {
	// Read the response
	cnt := 0
	for {
		beMsg, err := serverEnd.Receive()
		if err != nil {
			return fmt.Errorf("error while receiving from server: %w", err)
		}
		cnt++

		switch typedMsg := beMsg.(type) {
		// Read all till ReadyForQuery
		case *pgproto3.ReadyForQuery:
			cc.handle.Send(typedMsg)
			if err := cc.handle.Flush(); err != nil {
				return fmt.Errorf("error while flushing to client: %w", err)
			}
			cc.txStatus = typedMsg.TxStatus

			// Releasing the conn back to the pool
			if cc.txStatus == StatusIdle {
				cc.pool.ReleaseConn(cc.serverConn)
				cc.serverConn = nil
			}
			return nil
		default:
			cc.handle.Send(beMsg)
			// Flush if we have queued too many messages
			if cnt%10 == 0 {
				if err := cc.handle.Flush(); err != nil {
					return fmt.Errorf("error while flushing to client: %w", err)
				}
			}
			continue
		}
	}
}
