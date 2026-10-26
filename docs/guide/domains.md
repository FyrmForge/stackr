# Domains and DNS

Without a DNS connector you point each host at its ingress node yourself, and each host gets its own certificate. With one, stackr writes the records and gets wildcard certificates for the domains you pick. Namecheap is the only provider for now.

## Namecheap API access

Namecheap only opens its API to accounts that meet one of these:

- 20 or more domains on the account
- $50 or more on the account balance
- $50 or more spent in the last 2 years

Then:

1. In Namecheap, open Profile, Tools, Business & Dev Tools, Namecheap API Access.
2. Turn API access on and copy the API key.
3. Add the panel server's public IPv4 to the allow list. Calls from any other address are refused.

The key controls the whole Namecheap account: domains, contacts, renewals. Treat it like the account password.

## Add the connector

In the panel, open Admin, Connectors, Add a DNS connector: Namecheap. Fill in the username and API key. The server IPv4 is the one to allow list. It is filled in when the panel's box has a public IPv4 of its own, or behind NAT when an outside worker has seen the router's address. Otherwise enter your router's public address. Connect checks the key and lists the account's domains. The setup wizard's Certificates step has the same form.

An org owner can add the org's own Namecheap account under the org's Connectors tab. Its domains are only ever that org's.

A connector is only added in the panel: Server settings, Connectors for the server's, or the org's Connectors tab for an org's. Some kinds sign in on the provider's site, which a command line can't do. Once added, the CLI reaches it:

```bash
stackr server connectors domains <connector>
```

An org owner uses `stackr org connectors` with the same verbs.

## Pick the domains stackr manages

Each domain on the account shows one of three states:

- **Managed:** stackr writes its records and certificates.
- **Not managed:** press Manage to hand it to stackr.
- **DNS elsewhere:** the domain is registered at Namecheap but its DNS is hosted somewhere else. stackr can only manage domains on Namecheap BasicDNS. Take over DNS switches the domain to Namecheap's DNS; you type the domain to confirm. Records at the old DNS host stop applying, so copy any you need first.

A managed domain on a server connector says which orgs may use it under **Available to**. None ticked means every org, which is the default.

```bash
stackr server connectors manage <connector> example.com
stackr server connectors unmanage <connector> example.com
stackr server connectors takeover <connector> example.com --confirm example.com
```

To manage only part of a domain, give the name below it. stackr then writes only at and under `stackr.example.com` and never touches the rest. The plan lists the records it leaves alone. The domain list shows `managed under stackr.example.com`.

```bash
stackr server connectors plan <connector> example.com --under stackr
stackr server connectors manage <connector> example.com --under stackr
```

Stop managing, or removing the connector, is refused while a tile uses the domain; the message names the tiles. Once nothing uses it, stackr deletes only the records it created and forgets the domain.

## Records

Give a tile a domain from its drawer: pick a managed domain from the list and type the subdomain, or leave it empty for the domain itself. Other (I'll set DNS myself) takes any host, as before.

For each host stackr writes an A record to the public IPv4 of the ingress node that serves it, TTL 300 seconds. It adds an AAAA record only when the node has a public IPv6. When ingress moves, the records move with it. When the domain goes, stackr deletes them.

stackr only touches records it created. A name that already has a record it did not create is refused, with **Replace it** behind a confirm. A replaced record becomes stackr's: it is repointed and deleted like the others.

If a records update fails, Admin, Connectors shows the error over the DNS connectors. stackr tries again on the next change.

## Certificates

For a managed domain stackr gets wildcard certificates through DNS. A wildcard covers one level only, so stackr gets one per level in use:

- Typed one-level names like `shop.example.com` share `*.example.com`.
- Generated names like `api.shop.acme.example.com` share `*.shop.acme.example.com`, issued when a tile there first needs it.

The domain itself and anything else gets its own certificate. The panel's domain always keeps its own certificate, even under a managed domain. Hosts you point yourself keep per-host certificates too.

## Node public IP

Records point at the ingress node's detected public IP. To point them somewhere else, such as a floating IP or a load balancer, set the node's public IP: Admin, Nodes, the node, Network, Public IP. Empty goes back to the detected address.

```bash
stackr node set vps-fsn1 --public-ip 203.0.113.10
```

A node's own DNS name is its address: Admin, Nodes, the node, Network, Address. stackr writes its record when the name sits in a managed domain. Empty clears it.

```bash
stackr node set vps-fsn1 --address fsn1.example.com
```

A node behind NAT has no public address of its own, and stackr holds its records until it has one. Set its public IP to the router's address, with ports 80 and 443 forwarded to the node.

On Hetzner, a floating IP must also be configured on the box's network interface, or traffic to it never reaches the node. Assigning it in the Hetzner console is not enough.

## When an ingress node is down

stackr does not move records on its own when a node goes down. The node's page lists the domains it serves, each with **Move ingress**, which opens the env's Placement tab. Pick another ingress there; the records follow. The env page shows the same warning while its ingress is down.

See [Nodes](nodes.md#ingress-and-egress) for ingress picks.
