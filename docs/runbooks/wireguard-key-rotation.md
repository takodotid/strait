# WireGuard key rotation

Rotate the WireGuard private key of one site. Rotate one site at a time.

::: warning Finish steps 2 to 4 within the time budget
The time from step 2 to step 4 must stay under the smallest `unknownHold` of the names with a member at either site, unless another link carries the traffic. Past it, a name routes nowhere. Do steps 2 to 4 in one sitting. [What happens](#what-happens) says why.
:::

Run each write first with `--dry-run=server`, then without it. The examples show the dry run.

## Steps

The site is `a`, its peer `b`, the Secret `strait-wireguard` in `strait-system`. The private key goes from `wg genkey` into the Secret, and is never printed or written to a file. Run steps 1 to 4 in the same shell, because step 3 needs `OLD_KEY` from step 1 and step 4 needs `A_KEY` from step 3.

### 1. Check the links

List the links, and keep the old public key to compare in step 3.

```sh
strait --context site-a links
OLD_KEY=$(kubectl --context site-a get site a -o jsonpath='{.status.wireGuardPublicKey}')
```

Check: a link other than WireGuard is `HEALTHY` for each peer.

### 2. Replace the key

The time limit starts here.

```sh
wg genkey | kubectl --context site-a -n strait-system create secret generic strait-wireguard \
  --from-file=privateKey=/dev/stdin --dry-run=client -o yaml \
  | kubectl --context site-a -n strait-system replace --dry-run=server -f -
```

Check: the dry run prints `secret/strait-wireguard replaced (server dry run)`. The Secret keeps its name, which the chart's RBAC names. Then run it again without `--dry-run=server`.

### 3. Read the new public key

Read `Site.status.wireGuardPublicKey` until it differs from the old one. The new key appears within 2s of step 2; if the loop runs longer, interrupt it and check step 2.

```sh
A_KEY=$OLD_KEY
while [ "$A_KEY" = "$OLD_KEY" ]; do
  sleep 1
  A_KEY=$(kubectl --context site-a get site a -o jsonpath='{.status.wireGuardPublicKey}')
done
```

### 4. Update each peer

At each peer, set the `SitePeer` of site a to the new public key.

```sh
kubectl --context site-b patch sitepeer a --type merge \
  -p "{\"spec\":{\"wireguard\":{\"publicKey\":\"${A_KEY}\"}}}" --dry-run=server
```

### 5. Check both sites

Run the checks of [Upgrade, step 3](upgrade.md#_3-wait) at both sites.

Check: each link `HEALTHY`, and each peer `REACHABLE`. If a link stays unhealthy: [Troubleshooting, the `Healthy` condition](../troubleshooting.md#the-healthy-condition-of-a-sitelink). Why each peer must be updated: [RFC0007, Limitations](../../rfcs/RFC0007.md#limitations).

## What happens

| When | What happens |
|---|---|
| The site's Secret holds the new key | Within 2s, the active gateway agent puts the new key on every WireGuard interface, and publishes the new public key in `Site.status.wireGuardPublicKey`. |
| Until a peer's `SitePeer` holds the new public key | The WireGuard links between the site and that peer fail, in both directions: each end rejects the other's key. |
| A peer's `SitePeer` holds the new public key | That peer's agent applies it at once. The link passes its health checks again after the next handshake. |
