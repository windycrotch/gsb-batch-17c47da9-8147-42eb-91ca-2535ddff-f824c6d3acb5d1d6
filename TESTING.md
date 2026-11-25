# Testing

The reliability of the stream lifecycle (publisher re-publish, multi-node
relay rebuild, subscription / forwarding cleanup) is covered by automated
end-to-end tests that run locally: no redis server, no STUN server and no
internet access are required.

## How to run

```
go test -race -v ./...
```

The tests spawn real nodes in-process, serve the signaling websocket on a
local `httptest` server, and use real WebRTC peer connections (loopback ICE
candidates, `STUN_SERVER=NONE`). Multi-node scenarios link two nodes with an
in-process hook that replaces the redis pub/sub bus
(`WebRTC_CDN_Node.redisSendHook`).

## Scenarios

| Test | What it verifies |
| ---- | ---------------- |
| `TestSingleNodeRepublish` | Publisher disconnects and re-publishes the same stream-id on a single node. The waiting viewer receives STANDBY, then a new OFFER, and gets connected again. The node ends with exactly one source and one sink. |
| `TestRepublishSameConnection` | Publisher re-publishes the same stream-id on the **same** websocket (new request-id). The old source is replaced (not coexisting), the old request receives CLOSE, and the viewer renegotiates. This scenario used to deadlock the node. |
| `TestStaleSourceCleanupKeepsNewSource` | A late cleanup of a replaced source must not delete the newly registered source. |
| `TestStaleRelayCleanupKeepsNewRelay` | A late cleanup of a replaced relay must not delete the rebuilt relay. |
| `TestMultiNodeRemoteRepublish` | A viewer on node B watches a stream from node A. When the publisher re-publishes on A, B rebuilds its relay and the viewer receives a new OFFER. Final state: one source on A, one sender on A, one relay on B (a new relay instance, not the stale one). |
| `TestLastViewerLeavesReleasesRelay` | When the last viewer sends CLOSE, the sink, the relay on B and the sender on A are all released. Duplicate CLOSE and late ANSWER/CANDIDATE messages are ignored without corrupting counters or panicking. |
| `TestViewerDisconnectReleasesResources` | Same as above, but the viewer's websocket is closed abruptly (no CLOSE message). |
| `TestViewerRecoversFromStaleDiscovery` | A discovery (INFO) message points to a node that never answers. The relay watchdog (`relayReadyTimeout`) closes the stuck relay, the stream is re-resolved, and the viewer eventually receives an OFFER from the real node. |
| `TestConcurrentCloseAndLateSignaling` | Concurrent CLOSE (duplicated), websocket disconnects and late ANSWER/CANDIDATE on both publisher and viewer. All resources are released and connections deregistered. Run with `-race` to catch data races. |

## Notes for running offline

If the machine has no Go module cache and no internet access, point Go to an
existing module cache and disable downloads:

```
GOPROXY=off GOMODCACHE=/path/to/go/pkg/mod go test -race -v ./...
```
