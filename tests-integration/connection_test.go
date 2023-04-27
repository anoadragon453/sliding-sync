package syncv3

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	slidingsync "github.com/matrix-org/sliding-sync"
	"github.com/matrix-org/sliding-sync/sync2"
	"github.com/matrix-org/sliding-sync/sync3"
	"github.com/matrix-org/sliding-sync/testutils"
	"github.com/matrix-org/sliding-sync/testutils/m"
	"github.com/tidwall/gjson"
)

// Test that if you hit /sync and give up, we only start 1 connection.
// Relevant for when large accounts hit /sync for the first time and then time-out locally, and then
// hit /sync again without a ?pos=
func TestMultipleConnsAtStartup(t *testing.T) {
	pqString := testutils.PrepareDBConnectionString()
	// setup code
	v2 := runTestV2Server(t)
	v3 := runTestServer(t, v2, pqString)
	defer v2.close()
	defer v3.close()
	roomID := "!a:localhost"
	v2.addAccount(alice, aliceToken)
	var res *sync3.Response
	var wg sync.WaitGroup
	wg.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer wg.Done()
		_, body, _ := v3.doV3Request(t, ctx, aliceToken, "", sync3.Request{
			Lists: map[string]sync3.RequestList{"a": {
				Ranges: sync3.SliceRanges{
					[2]int64{0, 10},
				},
				RoomSubscription: sync3.RoomSubscription{
					TimelineLimit: 10,
				},
			}},
		})
		if body != nil {
			// we got sent a response but should've got nothing as we knifed the connection
			t.Errorf("got unexpected response: %s", string(body))
		}
	}()
	// wait until the proxy is waiting on v2. As this is the initial sync, we won't have a conn yet.
	v2.waitUntilEmpty(t, alice)
	// interrupt the connection
	cancel()
	wg.Wait()
	// respond to sync v2
	v2.queueResponse(alice, sync2.SyncResponse{
		Rooms: sync2.SyncRoomsResponse{
			Join: v2JoinTimeline(roomEvents{
				roomID: roomID,
				state:  createRoomState(t, alice, time.Now()),
				events: []json.RawMessage{
					testutils.NewStateEvent(t, "m.room.topic", "", alice, map[string]interface{}{}),
				},
			}),
		},
	})

	// do another /sync
	res = v3.mustDoV3Request(t, aliceToken, sync3.Request{
		Lists: map[string]sync3.RequestList{"a": {
			Ranges: sync3.SliceRanges{
				[2]int64{0, 10},
			},
			RoomSubscription: sync3.RoomSubscription{
				TimelineLimit: 10,
			},
		}},
	})
	m.MatchResponse(t, res, m.MatchList("a", m.MatchV3Ops(
		m.MatchV3SyncOp(0, 0, []string{roomID}),
	)))
}

// Regression test for running the proxy server behind a reverse proxy.
// The problem: when using a reverse proxy and a client cancels a request, the reverse proxy may
// not terminate the upstream connection. If this happens, subsequent requests from the client will
// stack up in Conn behind a mutex until the cancelled request is processed. In reality, we want the
// cancelled request to be stopped entirely. Whilst we cannot force reverse proxies to terminate the
// connection, we _can_ check if there is an outstanding request holding the mutex, and if so, interrupt
// it at the application layer. This test ensures we do that.
func TestOutstandingRequestsGetCancelled(t *testing.T) {
	pqString := testutils.PrepareDBConnectionString()
	// setup code
	v2 := runTestV2Server(t)
	v3 := runTestServer(t, v2, pqString)
	defer v2.close()
	defer v3.close()
	// Rooms A,B gets sent to the client initially, then we will send a 2nd blocking request with a 3s timeout.
	// During this time, we will send a 3rd request with modified sort operations to ensure that the proxy can
	// return an immediate response. We _should_ see the response promptly as the blocking request gets cancelled.
	// If it takes seconds to process this request, that implies it got stacked up behind a previous request,
	// failing the test.
	roomA := "!a:localhost" // name is A, older timestamp
	roomB := "!b:localhost" // name is B, newer timestamp
	v2.addAccount(alice, aliceToken)
	v2.queueResponse(alice, sync2.SyncResponse{
		Rooms: sync2.SyncRoomsResponse{
			Join: v2JoinTimeline(roomEvents{
				roomID: roomA,
				state:  createRoomState(t, alice, time.Now()),
				events: []json.RawMessage{
					testutils.NewStateEvent(t, "m.room.name", "", alice, map[string]interface{}{"name": "A"}),
				},
			}, roomEvents{
				roomID: roomB,
				state:  createRoomState(t, alice, time.Now().Add(time.Hour)),
				events: []json.RawMessage{
					testutils.NewStateEvent(t, "m.room.name", "", alice, map[string]interface{}{"name": "B"}, testutils.WithTimestamp(time.Now().Add(time.Hour))),
				},
			}),
		},
	})
	// first request to get some data
	res := v3.mustDoV3Request(t, aliceToken, sync3.Request{
		Lists: map[string]sync3.RequestList{"a": {
			Ranges: sync3.SliceRanges{
				[2]int64{0, 1},
			},
			Sort: []string{sync3.SortByName}, // A,B
			RoomSubscription: sync3.RoomSubscription{
				TimelineLimit: 1,
			},
		}},
	})
	m.MatchResponse(t, res, m.MatchList("a", m.MatchV3Count(2), m.MatchV3Ops(
		m.MatchV3SyncOp(0, 1, []string{roomA, roomB}),
	)))
	// now we do a blocking request, and a few ms later do another request which can be satisfied
	// using the same pos
	pos := res.Pos
	waitTimeMS := 3000
	startTime := time.Now()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		res2 := v3.mustDoV3RequestWithPos(t, aliceToken, pos, sync3.Request{
			Lists: map[string]sync3.RequestList{"a": {
				Ranges: sync3.SliceRanges{
					[2]int64{0, 1},
				},
				Sort: []string{sync3.SortByRecency}, // B,A
				RoomSubscription: sync3.RoomSubscription{
					TimelineLimit: 1,
				},
			}},
		})
		// this will be the response for the cancelled request initially
		m.MatchResponse(t, res2, m.MatchNoV3Ops())
		// retry request with new pos and we should see the new data
		res2 = v3.mustDoV3RequestWithPos(t, aliceToken, res2.Pos, sync3.Request{
			Lists: map[string]sync3.RequestList{"a": {
				Ranges: sync3.SliceRanges{
					[2]int64{0, 1},
				},
				Sort: []string{sync3.SortByRecency}, // B,A
				RoomSubscription: sync3.RoomSubscription{
					TimelineLimit: 1,
				},
			}},
		})
		m.MatchResponse(t, res2, m.MatchList("a", m.MatchV3Count(2), m.MatchV3Ops(
			m.MatchV3InvalidateOp(0, 1),
			m.MatchV3SyncOp(0, 1, []string{roomB, roomA}),
		)))
		if time.Since(startTime) > time.Second {
			t.Errorf("took >1s to process request which should have been processed instantly, took %v", time.Since(startTime))
		}
	}()
	req := sync3.Request{
		Lists: map[string]sync3.RequestList{"a": {
			Ranges: sync3.SliceRanges{
				[2]int64{0, 1},
			},
		}},
	}
	req.SetTimeoutMSecs(waitTimeMS)
	res = v3.mustDoV3RequestWithPos(t, aliceToken, pos, req)
	if time.Since(startTime) > time.Second {
		t.Errorf("took >1s to process request which should have been interrupted before timing out, took %v", time.Since(startTime))
	}
	m.MatchResponse(t, res, m.MatchList("a", m.MatchV3Count(2)), m.MatchNoV3Ops())
	wg.Wait()
}

// Regression test to ensure that ?timeout= isn't reset when live events come in.
func TestConnectionTimeoutNotReset(t *testing.T) {
	pqString := testutils.PrepareDBConnectionString()
	// setup code
	v2 := runTestV2Server(t)
	v3 := runTestServer(t, v2, pqString)
	defer v2.close()
	defer v3.close()
	// One room gets sent v2 updates, one room does not. Room B gets updates, which, because
	// we are tracking alphabetically, causes those updates to not trigger a v3 response. This
	// used to reset the timeout though, so we will check to make sure it doesn't.
	roomA := "!a:localhost"
	roomB := "!b:localhost"
	v2.addAccount(alice, aliceToken)
	v2.queueResponse(alice, sync2.SyncResponse{
		Rooms: sync2.SyncRoomsResponse{
			Join: v2JoinTimeline(roomEvents{
				roomID: roomA,
				state:  createRoomState(t, alice, time.Now()),
				events: []json.RawMessage{
					testutils.NewStateEvent(t, "m.room.name", "", alice, map[string]interface{}{"name": "A"}),
				},
			},
				roomEvents{
					roomID: roomB,
					state:  createRoomState(t, alice, time.Now()),
					events: []json.RawMessage{
						testutils.NewStateEvent(t, "m.room.name", "", alice, map[string]interface{}{"name": "B"}),
					},
				}),
		},
	})
	// first request to get some data
	res := v3.mustDoV3Request(t, aliceToken, sync3.Request{
		Lists: map[string]sync3.RequestList{"a": {
			Ranges: sync3.SliceRanges{
				[2]int64{0, 0}, // first room only -> roomID
			},
			Sort: []string{sync3.SortByName},
			RoomSubscription: sync3.RoomSubscription{
				TimelineLimit: 1,
			},
		}},
	})
	m.MatchResponse(t, res, m.MatchList("a", m.MatchV3Count(2), m.MatchV3Ops(
		m.MatchV3SyncOp(0, 0, []string{roomA}),
	)))
	// 2nd request with a 1s timeout
	req := sync3.Request{
		Lists: map[string]sync3.RequestList{"a": {
			Ranges: sync3.SliceRanges{
				[2]int64{0, 0},
			},
		}},
	}
	req.SetTimeoutMSecs(1000) // 1s
	// inject 4 events 500ms apart - if we reset the timeout each time then we will return late
	go func() {
		time.Sleep(10 * time.Millisecond)
		ticker := time.NewTicker(500 * time.Millisecond)
		i := 0
		for range ticker.C {
			if i > 3 {
				t.Logf("stopping update")
				break
			}
			t.Logf("sending update")
			v2.queueResponse(alice, sync2.SyncResponse{
				Rooms: sync2.SyncRoomsResponse{
					Join: v2JoinTimeline(roomEvents{
						roomID: roomB,
						events: []json.RawMessage{
							testutils.NewEvent(
								t, "m.room.message", alice, map[string]interface{}{"old": "msg"}, testutils.WithTimestamp(time.Now().Add(-2*time.Hour)),
							),
						},
					}),
				},
			})
			i++
		}
	}()
	startTime := time.Now()
	res = v3.mustDoV3RequestWithPos(t, aliceToken, res.Pos, req)
	dur := time.Since(startTime)
	if dur > (1500 * time.Millisecond) { // 0.5s leeway
		t.Fatalf("request took %v to complete, expected ~1s", dur)
	}
	m.MatchResponse(t, res, m.MatchList("a", m.MatchV3Count(2)), m.MatchNoV3Ops())

}

// Test that the txn_id is echoed back
func TestTxnIDEcho(t *testing.T) {
	pqString := testutils.PrepareDBConnectionString()
	// setup code
	v2 := runTestV2Server(t)
	v3 := runTestServer(t, v2, pqString)
	defer v2.close()
	defer v3.close()
	roomID := "!a:localhost"
	txnID := "hi"
	v2.addAccount(alice, aliceToken)
	v2.queueResponse(alice, sync2.SyncResponse{})

	res := v3.mustDoV3Request(t, aliceToken, sync3.Request{
		TxnID: txnID,
		RoomSubscriptions: map[string]sync3.RoomSubscription{
			roomID: {
				TimelineLimit: 5,
			},
		},
	})
	m.MatchResponse(t, res, m.MatchTxnID(txnID))

	txnID2 := "goodbyte"
	res = v3.mustDoV3RequestWithPos(t, aliceToken, res.Pos, sync3.Request{
		TxnID: txnID2,
		RoomSubscriptions: map[string]sync3.RoomSubscription{
			roomID: {
				TimelineLimit: 15,
			},
		},
	})
	m.MatchResponse(t, res, m.MatchTxnID(txnID2))

	// retry same request
	res = v3.mustDoV3RequestWithPos(t, aliceToken, res.Pos, sync3.Request{
		TxnID: txnID2,
		RoomSubscriptions: map[string]sync3.RoomSubscription{
			roomID: {
				TimelineLimit: 15,
			},
		},
	})
	m.MatchResponse(t, res, m.MatchTxnID(txnID2))
}

// Test that we implement https://github.com/matrix-org/matrix-spec-proposals/blob/kegan/sync-v3/proposals/3575-sync.md#sticky-request-parameters
// correctly server-side. We do this by:
// - Create rooms A, B and C.
// - Send a request with room_name_like = C. Get back pos=1
// - Send a request with pos=1 to filter for room_name_like = A . Discard the response.
// - Send a request with pos=1 to filter for room_name_like = B. Ensure we see both A,B and the txn_id is set correctly for both.
func TestTxnIDResponseBuffering(t *testing.T) {
	pqString := testutils.PrepareDBConnectionString()
	// setup code
	v2 := runTestV2Server(t)
	v3 := runTestServer(t, v2, pqString)
	defer v2.close()
	defer v3.close()
	roomA := "!a:localhost"
	roomB := "!b:localhost"
	roomC := "!c:localhost"
	v2.addAccount(alice, aliceToken)
	v2.queueResponse(alice, sync2.SyncResponse{
		Rooms: sync2.SyncRoomsResponse{
			Join: v2JoinTimeline(roomEvents{
				roomID: roomA,
				state:  createRoomState(t, alice, time.Now()),
				events: []json.RawMessage{
					testutils.NewStateEvent(t, "m.room.name", "", alice, map[string]interface{}{"name": "A"}),
				},
			},
				roomEvents{
					roomID: roomB,
					state:  createRoomState(t, alice, time.Now()),
					events: []json.RawMessage{
						testutils.NewStateEvent(t, "m.room.name", "", alice, map[string]interface{}{"name": "B"}),
					},
				},
				roomEvents{
					roomID: roomC,
					state:  createRoomState(t, alice, time.Now()),
					events: []json.RawMessage{
						testutils.NewStateEvent(t, "m.room.name", "", alice, map[string]interface{}{"name": "C"}),
					},
				}),
		},
	})
	// Send a request with room_name_like = C. Get back pos=1
	res := v3.mustDoV3Request(t, aliceToken, sync3.Request{
		TxnID: "c",
		Lists: map[string]sync3.RequestList{"a": {
			Ranges: sync3.SliceRanges{
				[2]int64{0, 10},
			},
			Sort: []string{sync3.SortByName},
			RoomSubscription: sync3.RoomSubscription{
				TimelineLimit: 1,
			},
			Filters: &sync3.RequestFilters{
				RoomNameFilter: "C",
			},
		}},
	})
	m.MatchResponse(t, res, m.MatchTxnID("c"), m.MatchList("a", m.MatchV3Count(1), m.MatchV3Ops(
		m.MatchV3SyncOp(0, 0, []string{roomC}),
	)))
	// Send a request with pos=1 to filter for room_name_like = A . Discard the response.
	v3.mustDoV3RequestWithPos(t, aliceToken, res.Pos, sync3.Request{
		TxnID: "a",
		Lists: map[string]sync3.RequestList{"a": {
			Ranges: sync3.SliceRanges{
				[2]int64{0, 10},
			},
			Sort: []string{sync3.SortByName},
			RoomSubscription: sync3.RoomSubscription{
				TimelineLimit: 1,
			},
			Filters: &sync3.RequestFilters{
				RoomNameFilter: "A",
			},
		}},
	})

	// Send a request with pos=1 to filter for room_name_like = B. Ensure we see both A,B and the txn_id is set correctly for both.
	res = v3.mustDoV3RequestWithPos(t, aliceToken, res.Pos, sync3.Request{
		TxnID: "b",
		Lists: map[string]sync3.RequestList{"a": {
			Ranges: sync3.SliceRanges{
				[2]int64{0, 10},
			},
			Sort: []string{sync3.SortByName},
			RoomSubscription: sync3.RoomSubscription{
				TimelineLimit: 1,
			},
			Filters: &sync3.RequestFilters{
				RoomNameFilter: "B",
			},
		}},
	})
	// this response should be the one for A
	m.MatchResponse(t, res, m.MatchTxnID("a"), m.MatchList("a", m.MatchV3Count(1), m.MatchV3Ops(
		m.MatchV3InvalidateOp(0, 0),
		m.MatchV3SyncOp(0, 0, []string{roomA}),
	)))

	// poll again
	res = v3.mustDoV3RequestWithPos(t, aliceToken, res.Pos, sync3.Request{})

	// now we get the response for B
	m.MatchResponse(t, res, m.MatchTxnID("b"), m.MatchList("a", m.MatchV3Count(1), m.MatchV3Ops(
		m.MatchV3InvalidateOp(0, 0),
		m.MatchV3SyncOp(0, 0, []string{roomB}),
	)))
}

// Regression test to make sure that if Alice does an initial sync followed by Bob, that Bob actually
// makes the request and can be serviced before Alice even though she was first. Early proxy impls had
// this behaviour but it regressed when we converted to a pubsub model as a single goroutine would handle
// EnsurePolling requests, rather than the HTTP goroutine.
func TestEnsurePollingDoesntQueue(t *testing.T) {
	pqString := testutils.PrepareDBConnectionString()
	// setup code
	v2 := runTestV2Server(t)
	v2.timeToWaitForV2Response = 5 * time.Second
	v3 := runTestServer(t, v2, pqString)
	defer v2.close()
	defer v3.close()
	roomA := "!a:localhost"
	roomB := "!b:localhost"
	v2.addAccount(alice, aliceToken)
	v2.addAccount(bob, bobToken)
	v2.queueResponse(bob, sync2.SyncResponse{
		Rooms: sync2.SyncRoomsResponse{
			Join: v2JoinTimeline(roomEvents{
				roomID: roomB,
				state:  createRoomState(t, bob, time.Now()),
				events: []json.RawMessage{
					testutils.NewStateEvent(t, "m.room.name", "", bob, map[string]interface{}{"name": "B"}),
				},
			}),
		},
	})
	var mu sync.Mutex
	aliceReturned := false
	// wait until alice makes the v2 /sync request, then start bob's v3 request
	go func() {
		t.Logf("waiting for alice's v2 poller to start")
		v2.waitUntilEmpty(t, alice) // alice's poller is making the v2 request
		t.Logf("alice's v2 poller is waiting, doing bob's v3 request")
		startTime := time.Now()
		res := v3.mustDoV3Request(t, bobToken, sync3.Request{ // start bob's v3 request
			RoomSubscriptions: map[string]sync3.RoomSubscription{
				roomB: {
					TimelineLimit: 1,
				},
			},
		})
		t.Logf("bob's v3 response returned")
		if time.Since(startTime) > 4*time.Second {
			t.Errorf("took too long to process bob's v3 request, it probably stacked behind alice")
		}
		mu.Lock()
		if aliceReturned {
			t.Errorf("Alice's /sync request returned before Bob's, expected Bob's to return first")
		}
		mu.Unlock()
		m.MatchResponse(t, res, m.MatchRoomSubscriptionsStrict(map[string][]m.RoomMatcher{
			roomB: {
				m.MatchJoinCount(1),
				m.MatchRoomName("B"),
			},
		}))
		// now send alice's response to unblock her
		t.Logf("sending alice's v2 response")
		v2.queueResponse(alice, sync2.SyncResponse{
			Rooms: sync2.SyncRoomsResponse{
				Join: v2JoinTimeline(roomEvents{
					roomID: roomA,
					state:  createRoomState(t, alice, time.Now()),
					events: []json.RawMessage{
						testutils.NewStateEvent(t, "m.room.name", "", alice, map[string]interface{}{"name": "A"}),
					},
				}),
			},
		})
	}()
	t.Logf("starting alice's v3 request")
	res := v3.mustDoV3Request(t, aliceToken, sync3.Request{
		RoomSubscriptions: map[string]sync3.RoomSubscription{
			roomA: {
				TimelineLimit: 1,
			},
		},
	})
	t.Logf("alice's v3 response returned")
	mu.Lock()
	aliceReturned = true
	mu.Unlock()
	m.MatchResponse(t, res, m.MatchRoomSubscriptionsStrict(map[string][]m.RoomMatcher{
		roomA: {
			m.MatchJoinCount(1),
			m.MatchRoomName("A"),
		},
	}))
}

// Test to ensure that we send back a spec-compliant error message when the session is expired.
func TestSessionExpiry(t *testing.T) {
	pqString := testutils.PrepareDBConnectionString()
	v2 := runTestV2Server(t)
	v2.addAccount(alice, aliceToken)
	v3 := runTestServer(t, v2, pqString)
	roomID := "!doesnt:matter"
	res1 := v3.mustDoV3Request(t, aliceToken, sync3.Request{
		RoomSubscriptions: map[string]sync3.RoomSubscription{
			roomID: {
				TimelineLimit: 1,
			},
		},
	})
	req := sync3.Request{}
	req.SetTimeoutMSecs(1)
	res2 := v3.mustDoV3RequestWithPos(t, aliceToken, res1.Pos, req)
	_ = v3.mustDoV3RequestWithPos(t, aliceToken, res2.Pos, req)
	// now use an earlier ?pos= to expire the session
	_, body, code := v3.doV3Request(t, context.Background(), aliceToken, res1.Pos, req)
	if code != 400 {
		t.Errorf("got HTTP %d want 400", code)
	}
	if gjson.ParseBytes(body).Get("errcode").Str != "M_UNKNOWN_POS" {
		t.Errorf("got %v want errcode=M_UNKNOWN_POS", string(body))
	}
}

func TestSessionExpiryOnBufferFill(t *testing.T) {
	roomID := "!doesnt:matter"
	maxPendingEventUpdates := 3
	pqString := testutils.PrepareDBConnectionString()
	v2 := runTestV2Server(t)
	v2.addAccount(alice, aliceToken)
	v2.queueResponse(alice, sync2.SyncResponse{
		Rooms: sync2.SyncRoomsResponse{
			Join: v2JoinTimeline(roomEvents{
				roomID: roomID,
				state:  createRoomState(t, alice, time.Now()),
				events: []json.RawMessage{
					testutils.NewStateEvent(t, "m.room.name", "", alice, map[string]interface{}{"name": "B"}),
				},
			}),
		},
	})
	v3 := runTestServer(t, v2, pqString, slidingsync.Opts{
		MaxPendingEventUpdates: maxPendingEventUpdates,
	})

	res := v3.mustDoV3Request(t, aliceToken, sync3.Request{
		RoomSubscriptions: map[string]sync3.RoomSubscription{
			roomID: {
				TimelineLimit: 1,
			},
		},
	})
	m.MatchResponse(t, res, m.MatchRoomSubscriptionsStrict(map[string][]m.RoomMatcher{
		roomID: {
			m.MatchJoinCount(1),
		},
	}))

	// inject maxPendingEventUpdates+1 events to expire the session
	events := make([]json.RawMessage, maxPendingEventUpdates+1)
	for i := range events {
		events[i] = testutils.NewEvent(t, "m.room.message", alice, map[string]interface{}{
			"msgtype": "m.text",
			"body":    fmt.Sprintf("Test %d", i),
		}, testutils.WithTimestamp(time.Now().Add(time.Duration(i)*time.Second)))
	}
	v2.queueResponse(alice, sync2.SyncResponse{
		Rooms: sync2.SyncRoomsResponse{
			Join: v2JoinTimeline(roomEvents{
				roomID: roomID,
				events: events,
			}),
		},
	})
	v2.waitUntilEmpty(t, aliceToken)

	_, body, code := v3.doV3Request(t, context.Background(), aliceToken, res.Pos, sync3.Request{})
	if code != 400 {
		t.Errorf("got HTTP %d want 400", code)
	}
	if gjson.ParseBytes(body).Get("errcode").Str != "M_UNKNOWN_POS" {
		t.Errorf("got %v want errcode=M_UNKNOWN_POS", string(body))
	}
}

func TestExpiredAccessToken(t *testing.T) {
	pqString := testutils.PrepareDBConnectionString()
	v2 := runTestV2Server(t)
	v2.addAccount(alice, aliceToken)
	v3 := runTestServer(t, v2, pqString)
	roomID := "!doesnt:matter"
	res := v3.mustDoV3Request(t, aliceToken, sync3.Request{
		RoomSubscriptions: map[string]sync3.RoomSubscription{
			roomID: {
				TimelineLimit: 1,
			},
		},
	})
	// now expire the token
	v2.invalidateToken(aliceToken)
	// now do another request, this should 401 as the auth token has expired
	req := sync3.Request{}
	req.SetTimeoutMSecs(1)
	_, body, statusCode := v3.doV3Request(t, context.Background(), aliceToken, res.Pos, req)
	if statusCode != 401 {
		t.Fatalf("got %d want 401 : %v", statusCode, string(body))
	}
}
