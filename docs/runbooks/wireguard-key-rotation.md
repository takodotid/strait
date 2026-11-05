# WireGuard key rotation

Rotate the WireGuard private key of one site. Rotate one site at a time.

## What happens

| When | What happens |
|---|---|
| The site's Secret holds the new key | Within 2s, the active gateway agent puts the new key on every WireGuard interface, and publishes the new public key in `Site.status.wireGuardPublicKey`. |
| Until a peer's `SitePeer` holds the new public key | The WireGuard links between the site and that peer fail, in both directions: each end rejects the other's key. |
| A peer's `SitePeer` holds the new public key | That peer's agent applies it at once. The link passes its health checks again after the next handshake. |

While the WireGuard links to a peer fail, traffic and the observation exchange take another healthy link to that peer, such as a `direct` link. With no other healthy link, each of the two sites counts the other's members `Unknown`: a routed member is held for `policy.unknownHold`, then the name routes nowhere. So the time from step 2 to step 4 must stay under the smallest `unknownHold` of the names with a member at either site, unless another link carries the traffic.

## Steps

The site is `a`, its peer `b`, the Secret `strait-wireguard` in `strait-system`. The private key goes from `wg genkey` into the Secret, and is never printed or written to a file. Public keys are not secrets.

| Step | What to do | Check |
|---|---|---|
| 1. Check the links | `strait --context site-a links`, and keep the old public key to compare in step 3. | Whether a link other than WireGuard is `HEALTHY` for each peer. If not, plan steps 2 to 4 within the budget above. |
| 2. Replace the key | Run the dry run, then the same command without `--dry-run=server`. | The dry run prints `secret/strait-wireguard replaced (server dry run)`. The Secret keeps its name, which the chart's RBAC names. |
| 3. Read the new public key | Read `Site.status.wireGuardPublicKey` until it differs from the old one. | |
| 4. Update each peer | At each peer, set the `SitePeer` of site a to the new public key. | |
| 5. Check | `strait links` and `strait peers` at both sites. | Each link `HEALTHY`, and each peer `REACHABLE`. |

```sh
# 1. The links, and the old public key.
strait --context site-a links
OLD_KEY=$(kubectl --context site-a get site a -o jsonpath='{.status.wireGuardPublicKey}')

# 2. Replace the private key.
wg genkey | kubectl --context site-a -n strait-system create secret generic strait-wireguard \
  --from-file=privateKey=/dev/stdin --dry-run=client -o yaml \
  | kubectl --context site-a -n strait-system replace --dry-run=server -f -
wg genkey | kubectl --context site-a -n strait-system create secret generic strait-wireguard \
  --from-file=privateKey=/dev/stdin --dry-run=client -o yaml \
  | kubectl --context site-a -n strait-system replace -f -

# 3. The new public key.
A_KEY=$OLD_KEY
while [ "$A_KEY" = "$OLD_KEY" ]; do
  sleep 1
  A_KEY=$(kubectl --context site-a get site a -o jsonpath='{.status.wireGuardPublicKey}')
done

# 4. At each peer.
kubectl --context site-b patch sitepeer a --type merge \
  -p "{\"spec\":{\"wireguard\":{\"publicKey\":\"${A_KEY}\"}}}" --dry-run=server
kubectl --context site-b patch sitepeer a --type merge \
  -p "{\"spec\":{\"wireguard\":{\"publicKey\":\"${A_KEY}\"}}}"

# 5. Both ends.
strait --context site-a links
strait --context site-b links
```

`kubectl replace` writes the Secret as given. `kubectl apply` would also copy the key into the annotation `kubectl.kubernetes.io/last-applied-configuration`.

If a link stays unhealthy: [Troubleshooting, the `Healthy` condition](../troubleshooting.md#unreachable-peers-and-unhealthy-links). Why each peer must be updated: [RFC0007, Limitations](../../rfcs/RFC0007.md#limitations).
