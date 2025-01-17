package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/benbjohnson/clock"
	ipns "github.com/ipfs/boxo/ipns"
	"github.com/ipfs/boxo/routing/http/contentrouter"
	"github.com/ipfs/boxo/routing/http/internal/drjson"
	"github.com/ipfs/boxo/routing/http/types"
	"github.com/ipfs/boxo/routing/http/types/iter"
	jsontypes "github.com/ipfs/boxo/routing/http/types/json"
	"github.com/ipfs/boxo/routing/http/types/ndjson"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

var (
	_                 contentrouter.Client = &Client{}
	logger                                 = logging.Logger("routing/http/client")
	defaultHTTPClient                      = &http.Client{
		Transport: &ResponseBodyLimitedTransport{
			RoundTripper: http.DefaultTransport,
			LimitBytes:   1 << 20,
			UserAgent:    defaultUserAgent,
		},
	}
)

const (
	mediaTypeJSON       = "application/json"
	mediaTypeNDJSON     = "application/x-ndjson"
	mediaTypeIPNSRecord = "application/vnd.ipfs.ipns-record"
)

type Client struct {
	baseURL    string
	httpClient httpClient
	clock      clock.Clock
	accepts    string

	peerID   peer.ID
	addrs    []types.Multiaddr
	identity crypto.PrivKey

	// Called immediately after signing a provide request. It is used
	// for testing, e.g., testing the server with a mangled signature.
	//lint:ignore SA1019 // ignore staticcheck
	afterSignCallback func(req *types.WriteBitswapRecord)
}

// defaultUserAgent is used as a fallback to inform HTTP server which library
// version sent a request
var defaultUserAgent = moduleVersion()

var _ contentrouter.Client = &Client{}

type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Option func(*Client)

func WithIdentity(identity crypto.PrivKey) Option {
	return func(c *Client) {
		c.identity = identity
	}
}

func WithHTTPClient(h httpClient) Option {
	return func(c *Client) {
		c.httpClient = h
	}
}

func WithUserAgent(ua string) Option {
	return func(c *Client) {
		if ua == "" {
			return
		}
		httpClient, ok := c.httpClient.(*http.Client)
		if !ok {
			return
		}
		transport, ok := httpClient.Transport.(*ResponseBodyLimitedTransport)
		if !ok {
			return
		}
		transport.UserAgent = ua
	}
}

func WithProviderInfo(peerID peer.ID, addrs []multiaddr.Multiaddr) Option {
	return func(c *Client) {
		c.peerID = peerID
		for _, a := range addrs {
			c.addrs = append(c.addrs, types.Multiaddr{Multiaddr: a})
		}
	}
}

func WithStreamResultsRequired() Option {
	return func(c *Client) {
		c.accepts = mediaTypeNDJSON
	}
}

// New creates a content routing API client.
// The Provider and identity parameters are option. If they are nil, the [client.ProvideBitswap] method will not function.
func New(baseURL string, opts ...Option) (*Client, error) {
	client := &Client{
		baseURL:    baseURL,
		httpClient: defaultHTTPClient,
		clock:      clock.New(),
		accepts:    strings.Join([]string{mediaTypeNDJSON, mediaTypeJSON}, ","),
	}

	for _, opt := range opts {
		opt(client)
	}

	if client.identity != nil && client.peerID.Size() != 0 && !client.peerID.MatchesPublicKey(client.identity.GetPublic()) {
		return nil, errors.New("identity does not match provider")
	}

	return client, nil
}

// measuringIter measures the length of the iter and then publishes metrics about the whole req once the iter is closed.
// Of course, if the caller forgets to close the iter, this won't publish anything.
type measuringIter[T any] struct {
	iter.Iter[T]
	ctx context.Context
	m   *measurement
}

func (c *measuringIter[T]) Next() bool {
	c.m.length++
	return c.Iter.Next()
}

func (c *measuringIter[T]) Val() T {
	return c.Iter.Val()
}

func (c *measuringIter[T]) Close() error {
	c.m.record(c.ctx)
	return c.Iter.Close()
}

// FindProviders searches for providers that are able to provide the given [cid.Cid].
// In a more generic way, it is also used as a mapping between CIDs and relevant metadata.
func (c *Client) FindProviders(ctx context.Context, key cid.Cid) (providers iter.ResultIter[types.Record], err error) {
	// TODO test measurements
	m := newMeasurement("FindProviders")

	url := c.baseURL + "/routing/v1/providers/" + key.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", c.accepts)

	m.host = req.Host

	start := c.clock.Now()
	resp, err := c.httpClient.Do(req)

	m.err = err
	m.latency = c.clock.Since(start)

	if err != nil {
		m.record(ctx)
		return nil, err
	}

	m.statusCode = resp.StatusCode
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		m.record(ctx)
		return iter.FromSlice[iter.Result[types.Record]](nil), nil
	}

	if resp.StatusCode != http.StatusOK {
		err := httpError(resp.StatusCode, resp.Body)
		resp.Body.Close()
		m.record(ctx)
		return nil, err
	}

	respContentType := resp.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(respContentType)
	if err != nil {
		resp.Body.Close()
		m.err = err
		m.record(ctx)
		return nil, fmt.Errorf("parsing Content-Type: %w", err)
	}

	m.mediaType = mediaType

	var skipBodyClose bool
	defer func() {
		if !skipBodyClose {
			resp.Body.Close()
		}
	}()

	var it iter.ResultIter[types.Record]
	switch mediaType {
	case mediaTypeJSON:
		parsedResp := &jsontypes.ProvidersResponse{}
		err = json.NewDecoder(resp.Body).Decode(parsedResp)
		var sliceIt iter.Iter[types.Record] = iter.FromSlice(parsedResp.Providers)
		it = iter.ToResultIter(sliceIt)
	case mediaTypeNDJSON:
		skipBodyClose = true
		it = ndjson.NewRecordsIter(resp.Body)
	default:
		logger.Errorw("unknown media type", "MediaType", mediaType, "ContentType", respContentType)
		return nil, errors.New("unknown content type")
	}

	return &measuringIter[iter.Result[types.Record]]{Iter: it, ctx: ctx, m: m}, nil
}

// Deprecated: protocol-agnostic provide is being worked on in [IPIP-378]:
//
// [IPIP-378]: https://github.com/ipfs/specs/pull/378
func (c *Client) ProvideBitswap(ctx context.Context, keys []cid.Cid, ttl time.Duration) (time.Duration, error) {
	if c.identity == nil {
		return 0, errors.New("cannot provide Bitswap records without an identity")
	}
	if c.peerID.Size() == 0 {
		return 0, errors.New("cannot provide Bitswap records without a peer ID")
	}

	ks := make([]types.CID, len(keys))
	for i, c := range keys {
		ks[i] = types.CID{Cid: c}
	}

	now := c.clock.Now()

	req := types.WriteBitswapRecord{
		Protocol: "transport-bitswap",
		Schema:   types.SchemaBitswap,
		Payload: types.BitswapPayload{
			Keys:        ks,
			AdvisoryTTL: &types.Duration{Duration: ttl},
			Timestamp:   &types.Time{Time: now},
			ID:          &c.peerID,
			Addrs:       c.addrs,
		},
	}
	err := req.Sign(c.peerID, c.identity)
	if err != nil {
		return 0, err
	}

	if c.afterSignCallback != nil {
		c.afterSignCallback(&req)
	}

	advisoryTTL, err := c.provideSignedBitswapRecord(ctx, &req)
	if err != nil {
		return 0, err
	}

	return advisoryTTL, err
}

// ProvideAsync makes a provide request to a delegated router
//
//lint:ignore SA1019 // ignore staticcheck
func (c *Client) provideSignedBitswapRecord(ctx context.Context, bswp *types.WriteBitswapRecord) (time.Duration, error) {
	//lint:ignore SA1019 // ignore staticcheck
	req := jsontypes.WriteProvidersRequest{Providers: []types.Record{bswp}}

	url := c.baseURL + "/routing/v1/providers/"

	b, err := drjson.MarshalJSONBytes(req)
	if err != nil {
		return 0, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewBuffer(b))
	if err != nil {
		return 0, err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return 0, fmt.Errorf("making HTTP req to provide a signed record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, httpError(resp.StatusCode, resp.Body)
	}

	//lint:ignore SA1019 // ignore staticcheck
	var provideResult jsontypes.WriteProvidersResponse
	err = json.NewDecoder(resp.Body).Decode(&provideResult)
	if err != nil {
		return 0, err
	}
	if len(provideResult.ProvideResults) != 1 {
		return 0, fmt.Errorf("expected 1 result but got %d", len(provideResult.ProvideResults))
	}

	//lint:ignore SA1019 // ignore staticcheck
	v, ok := provideResult.ProvideResults[0].(*types.WriteBitswapRecordResponse)
	if !ok {
		return 0, errors.New("expected AdvisoryTTL field")
	}

	if v.AdvisoryTTL != nil {
		return v.AdvisoryTTL.Duration, nil
	}

	return 0, nil
}

// FindPeers searches for information for the given [peer.ID].
func (c *Client) FindPeers(ctx context.Context, pid peer.ID) (peers iter.ResultIter[*types.PeerRecord], err error) {
	m := newMeasurement("FindPeers")

	url := c.baseURL + "/routing/v1/peers/" + peer.ToCid(pid).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", c.accepts)

	m.host = req.Host

	start := c.clock.Now()
	resp, err := c.httpClient.Do(req)

	m.err = err
	m.latency = c.clock.Since(start)

	if err != nil {
		m.record(ctx)
		return nil, err
	}

	m.statusCode = resp.StatusCode
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		m.record(ctx)
		return iter.FromSlice[iter.Result[*types.PeerRecord]](nil), nil
	}

	if resp.StatusCode != http.StatusOK {
		err := httpError(resp.StatusCode, resp.Body)
		resp.Body.Close()
		m.record(ctx)
		return nil, err
	}

	respContentType := resp.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(respContentType)
	if err != nil {
		resp.Body.Close()
		m.err = err
		m.record(ctx)
		return nil, fmt.Errorf("parsing Content-Type: %w", err)
	}

	m.mediaType = mediaType

	var skipBodyClose bool
	defer func() {
		if !skipBodyClose {
			resp.Body.Close()
		}
	}()

	var it iter.ResultIter[*types.PeerRecord]
	switch mediaType {
	case mediaTypeJSON:
		parsedResp := &jsontypes.PeersResponse{}
		err = json.NewDecoder(resp.Body).Decode(parsedResp)
		var sliceIt iter.Iter[*types.PeerRecord] = iter.FromSlice(parsedResp.Peers)
		it = iter.ToResultIter(sliceIt)
	case mediaTypeNDJSON:
		skipBodyClose = true
		it = ndjson.NewPeerRecordsIter(resp.Body)
	default:
		logger.Errorw("unknown media type", "MediaType", mediaType, "ContentType", respContentType)
		return nil, errors.New("unknown content type")
	}

	return &measuringIter[iter.Result[*types.PeerRecord]]{Iter: it, ctx: ctx, m: m}, nil
}

// GetIPNS tries to retrieve the [ipns.Record] for the given [ipns.Name]. The record is
// validated against the given name. If validation fails, an error is returned, but no
// record.
func (c *Client) GetIPNS(ctx context.Context, name ipns.Name) (*ipns.Record, error) {
	url := c.baseURL + "/routing/v1/ipns/" + name.String()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", mediaTypeIPNSRecord)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("making HTTP req to get IPNS record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp.StatusCode, resp.Body)
	}

	// Limit the reader to the maximum record size.
	rawRecord, err := io.ReadAll(io.LimitReader(resp.Body, int64(ipns.MaxRecordSize)))
	if err != nil {
		return nil, fmt.Errorf("making HTTP req to get IPNS record: %w", err)
	}

	record, err := ipns.UnmarshalRecord(rawRecord)
	if err != nil {
		return nil, fmt.Errorf("IPNS record from remote endpoint is not valid: %w", err)
	}

	err = ipns.ValidateWithName(record, name)
	if err != nil {
		return nil, fmt.Errorf("IPNS record from remote endpoint is not valid: %w", err)
	}

	return record, nil
}

// PutIPNS attempts at putting the given [ipns.Record] for the given [ipns.Name].
func (c *Client) PutIPNS(ctx context.Context, name ipns.Name, record *ipns.Record) error {
	url := c.baseURL + "/routing/v1/ipns/" + name.String()

	rawRecord, err := ipns.MarshalRecord(record)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(rawRecord))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", mediaTypeIPNSRecord)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("making HTTP req to get IPNS record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return httpError(resp.StatusCode, resp.Body)
	}

	return nil
}
