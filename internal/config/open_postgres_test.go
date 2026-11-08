package config

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// TestOpenPostgres_RefusesServerThatSkipsAuthentication checks that a URL
// with channel_binding=require or require_auth refuses a server that never
// authenticates. Such a server can be anyone on the network path when the
// URL uses sslmode=require, which encrypts but does not check the server's
// certificate. A connection to it would hand it every query and its
// parameters. The control row shows the fake server accepts a connection
// that asks for no authentication.
func TestOpenPostgres_RefusesServerThatSkipsAuthentication(t *testing.T) {
	cases := []struct {
		name        string
		params      string
		wantRefused bool
	}{
		{name: "no authentication required", params: "sslmode=require", wantRefused: false},
		{name: "channel_binding=require", params: "sslmode=require&channel_binding=require", wantRefused: true},
		{name: "require_auth=scram-sha-256", params: "sslmode=require&require_auth=scram-sha-256", wantRefused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := startNoAuthServer(t)
			url := "postgres://hardcount:secret@" + srv.addr + "/hardcount?" + tc.params
			db, err := OpenPostgres(DBConfig{URL: url, MaxOpenConns: 1, MaxIdleConns: 1, PingTimeout: 5 * time.Second})
			queries := srv.queries.Load()
			if err == nil {
				if cerr := db.Close(); cerr != nil {
					t.Errorf("close: %v", cerr)
				}
			}

			if tc.wantRefused {
				if err == nil || queries > 0 {
					t.Fatalf("OpenPostgres(%q) error = %v, server got %d queries; want a refused connection and 0 queries", url, err, queries)
				}
				t.Logf("refused: %v", err)
				return
			}
			if err != nil || queries == 0 {
				t.Fatalf("OpenPostgres(%q) error = %v, server got %d queries; want a connection and the ping query", url, err, queries)
			}
		})
	}
}

// noAuthServer is a fake Postgres server that accepts TLS and then skips
// authentication: it answers the startup message with AuthenticationOk and
// ReadyForQuery. It answers each simple query as empty and counts it.
type noAuthServer struct {
	addr    string
	tls     *tls.Config
	queries atomic.Int32
}

// startNoAuthServer listens on a free local port until the test ends.
func startNoAuthServer(t *testing.T) *noAuthServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &noAuthServer{
		addr: ln.Addr().String(),
		tls:  &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}},
	}

	// t.Context is cancelled before cleanups run. Closing the listener and
	// each connection then ends every goroutine, and the cleanup waits for
	// them so none outlives the test.
	var wg sync.WaitGroup
	context.AfterFunc(t.Context(), func() {
		_ = ln.Close() // a close error at test end changes nothing
	})
	t.Cleanup(wg.Wait)
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // the listener is closed
			}
			context.AfterFunc(t.Context(), func() {
				_ = conn.Close() // a close error at test end changes nothing
			})
			wg.Go(func() { srv.serve(conn) })
		}
	})
	return srv
}

// serve runs one client connection. It returns on any error, because the
// client hanging up is how a refusal looks from the server side.
func (s *noAuthServer) serve(raw net.Conn) {
	backend := pgproto3.NewBackend(raw, raw)
	msg, err := backend.ReceiveStartupMessage()
	if err != nil {
		return
	}
	if _, ok := msg.(*pgproto3.SSLRequest); !ok {
		return
	}
	if _, err := raw.Write([]byte("S")); err != nil {
		return
	}
	conn := tls.Server(raw, s.tls)
	backend = pgproto3.NewBackend(conn, conn)
	if _, err := backend.ReceiveStartupMessage(); err != nil {
		return
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := backend.Flush(); err != nil {
		return
	}
	for {
		msg, err := backend.Receive()
		if err != nil {
			return
		}
		switch msg.(type) {
		case *pgproto3.Query:
			s.queries.Add(1)
			backend.Send(&pgproto3.EmptyQueryResponse{})
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if err := backend.Flush(); err != nil {
				return
			}
		case *pgproto3.Terminate:
			return
		}
	}
}

// selfSignedCert makes a throwaway certificate for the fake server. With
// sslmode=require the client does not check it, as with a real attacker.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake-postgres"},
		NotBefore:    time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
