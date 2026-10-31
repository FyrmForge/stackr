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

`manage` takes over the existing records a tile already serves, as the panel ticks them. `--adopt=false` leaves them alone.

To manage only part of a domain, give the name below it. stackr then writes only at and under `stackr.example.com` and never touches the rest. The plan lists the records it leaves alone. The domain list shows `managed under stackr.example.com`.

```bash
stackr server connectors plan <connector> example.com --under stackr
stackr server connectors manage <connector> example.com --under stackr
```

### Change scope

Change scope on a managed domain picks again between all of it and only under a name. The domain stays managed and no tile gives up a hostname. stackr shows the plan first, then asks.

- **Widening** (only under `test` to all): records now inside that point at your nodes are offered for adoption, ticked when a tile serves them. stackr writes the records tiles now need.
- **Narrowing** (all to only under `test`): records stackr wrote outside the new scope become yours. They stay in the zone so sites keep answering; stackr stops updating them. The plan lists them first.

The domain resource stackr made for the domain moves with the scope, so generated names go under the new one. Change scope is refused, naming the hosts, while a generated or wildcard hostname would fall outside the new scope, or while tile domains are named after the resource that moves. Remove those domains first.

```bash
stackr server connectors rescope <connector> example.com --under test
stackr server connectors rescope <connector> example.com
```

Without `--under` it widens to the whole domain. `rescope` prints the plan and asks; `-y` skips the question. `--adopt=false` leaves the existing records alone.

Stop managing, or removing the connector, is refused while a tile uses the domain; the message names the tiles. Once nothing uses it, stackr deletes only the records it created and forgets the domain.

## Records

Give a tile a domain from its drawer: pick a managed domain from the list and type the subdomain, or leave it empty for the domain itself. Other (I'll set DNS myself) takes any host, as before.

For each host stackr writes an A record to the public IPv4 of the ingress node that serves it, TTL 300 seconds. It adds an AAAA record only when the node has a public IPv6. When ingress moves, the records move with it. When the domain goes, stackr deletes them.

stackr only touches records it created. A name that already has a record it did not create is refused, with **Replace it** behind a confirm. A replaced record becomes stackr's: it is repointed and deleted like the others.

If a records update fails, Admin, Connectors shows the error over the DNS connectors. stackr tries again on the next change.

## Custom records

You can keep extra records in stackr too: site verification TXT, mail (MX, SPF, DKIM, DMARC), a CNAME to an outside service. Types are TXT, MX, CNAME, A and AAAA, written with stackr's TTL of 300 seconds. They have two homes.

**On an app's hostname.** Add them in the drawer's Domains tab, under the hostname. A record sits on the hostname itself or a name below it (`_dmarc` under `shop.example.com` is `_dmarc.shop.example.com`), never on another app's name. They live and die with the hostname: removing the domain, deleting the app or a promote whose file drops it removes them too.

**On the domain itself.** Records no app owns, like an apex MX or a verification TXT. Open Records on the managed domain's row in the connector's settings.

Each record shows where it is written, for example "Managed by example.com via Namecheap (server)".

The rules:

- The name must be in a domain stackr manages, inside its scope, and the domain must be available to the app's org. Otherwise the record is refused, naming the host and the domain. Add such records at your DNS host.
- Values are checked before anything is written: an A is an IPv4 address, an AAAA an IPv6 one, a CNAME a host name, an MX a priority and a host (`10 mx.example.net`), a TXT one line of text.
- TXT, MX, A and AAAA go next to what the name already has. The answer says so: "added (2 others there stay)". The same record already made by hand is taken over ("taken over"), not doubled; from then on stackr deletes it with the hostname.
- A CNAME needs its name to itself. It is refused where the name has any other record, and nothing else is added where the name has a CNAME.
- A name is either an app hostname or a custom record, not both. Only TXT and MX can sit next to an app. This is refused both ways with the same message.
- If someone changes or deletes a custom record at the DNS host, stackr puts it back on the next records sync, and the record shows "Changed outside stackr; put back". The A records stackr writes for apps keep the old rule: a hand edit makes the record yours.
- Narrowing a domain's scope drops the domain's own custom records outside the new scope. Their records stay in the zone as yours.

From the CLI:

```bash
stackr tile domain records ls shop.example.com
stackr tile domain records add shop.example.com TXT "google-site-verification=abc"
stackr tile domain records add shop.example.com TXT "v=DMARC1; p=none" --name _dmarc
stackr tile domain records rm shop.example.com <id or value>

stackr server connectors records ls <connector> example.com
stackr server connectors records add <connector> example.com MX "10 mx.example.net"
stackr server connectors records rm <connector> example.com <id or value>
```

`stackr org connectors records` does the same for an org's own connector.

### In the stack file

A domain entry takes `records:` and `dns:`.

```yaml
domains:
  - host: shop.example.com
    records:
      - {type: TXT, value: "google-site-verification=..."}
      - {name: _dmarc, type: TXT, value: "v=DMARC1; p=none"}
      - {type: MX, value: "10 mx.example.net"}
  - host: api.example.com
    dns: managed
```

- `records:` is a list of `{name, type, value}`. `name` is relative to the host; left out, it is the host itself.
- `records:` means the host must be managed. `dns: managed` asks the same without records.
- The plan refuses a host no managed domain covers, a domain the org may not use, a bad value, a CNAME clash and a name another app holds. Each error names the host and the domain. The plan row says how each new record lands, for example "added (1 other there stays)".
- Export writes `records:` back. `dns: managed` is a plan check only and is not exported.

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
