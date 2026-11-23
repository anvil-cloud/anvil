# Security groups, from 0.0.0.0/0 to least privilege

> Draft. Audience: developers who deploy to AWS but haven't had to think hard about networking yet.

Security groups are the most-used and least-understood control in AWS. Every VPC resource has one, almost every tutorial opens one too wide, and the console makes "allow everything" a two-click affair. This post walks one database from the worst possible setup to the one Anvil builds for you, one step at a time, and explains what each step actually buys.

First, the groundwork: you can't reason about a security group without knowing what network it sits in.

---

## The groundwork

### What is a VPC?

A **Virtual Private Cloud** is your own private network inside an AWS region. You pick its address range (a CIDR block, e.g. `10.0.0.0/16`, which is 65,536 addresses), and nothing gets in or out unless you add the pieces that allow it. Resources inside it, such as EC2 instances, RDS databases, VPC-attached Lambdas and ECS tasks, get a network interface (an ENI) with a private IP from that range.

Think of it as an office building with no doors until you install them.

### The AWS public zone vs. your VPC

Not everything in AWS lives in a VPC. Services like S3, DynamoDB, SQS, SNS and the AWS APIs themselves live in what's often called the **AWS public zone**: they're reached at public endpoints (`sqs.ap-southeast-2.amazonaws.com`) over the internet or AWS's backbone, and they are protected by **IAM**, not by network rules. Anyone on the internet can reach the S3 endpoint; only callers with the right credentials get anything back.

Resources inside your VPC are the opposite: they are protected by **the network first**. A database with a private IP in a private subnet can't be reached from the internet at all, regardless of what credentials an attacker holds.

| | AWS public zone | Your VPC |
|---|---|---|
| Examples | S3, DynamoDB, SQS, IAM/STS APIs | RDS, EC2, ECS tasks, VPC Lambdas |
| Reachable from the internet? | Yes, by design | Only if you build a path |
| First line of defence | IAM (who are you?) | Network (can you even get here?) |

Why this matters: when credentials leak, a public-zone service is only as safe as its IAM policies. A VPC resource has a second wall. Good architecture uses both: network controls to decide *who can connect*, IAM and database auth to decide *what they can do once connected*.

### Subnets: public vs. private

A VPC's address range is split into **subnets**, each living in exactly one Availability Zone. The thing that makes a subnet "public" or "private" is not a checkbox. It's the **route table**:

- **Public subnet**: its route table has `0.0.0.0/0 → Internet Gateway`. Resources here *can* have a public IP and be reached from the internet.
- **Private subnet**: no route to an Internet Gateway. Resources here have only private IPs and cannot be reached from the internet, full stop. They may still reach *out* through a NAT Gateway.

```
VPC 10.0.0.0/16
├── public subnet  10.0.0.0/24   route: 0.0.0.0/0 → igw-...    (load balancers, NAT gateways)
└── private subnet 10.0.1.0/24   route: 0.0.0.0/0 → nat-...    (apps, databases)
```

Rule of thumb: the only things in a public subnet should be things whose *job* is to face the internet, such as load balancers and NAT gateways. Everything else goes private.

### Internet Gateway vs. NAT Gateway

Both connect a VPC to the internet, in very different ways.

| | Internet Gateway (IGW) | NAT Gateway |
|---|---|---|
| Direction | **Two-way.** Inbound and outbound | **Outbound only.** Responses come back; new inbound connections can't |
| Who uses it | Resources in public subnets with public IPs | Resources in private subnets |
| Where it lives | Attached to the VPC | Inside a public subnet, one per AZ for resilience |
| Cost | Free | Hourly charge per gateway plus per-GB processing |
| Analogy | The building's front door | A mail slot you can post letters out of; replies come back, strangers can't push in |

A private subnet behind a NAT can `npm install`, call Stripe, and pull a container image, but nobody on the internet can open a connection to it. That asymmetry is the whole point.

(If all your private resources need is other AWS services, VPC endpoints can replace the NAT entirely and keep that traffic off the internet. That's a post of its own.)

---

## Security groups

### What they are

A **security group** is a stateful, allow-only firewall attached to a **network interface**. Each rule says: allow this protocol, on this port range, from (inbound) or to (outbound) this source, where the source is a CIDR, a prefix list, or **another security group**.

The properties that matter:

- **Default deny.** Anything not explicitly allowed is dropped. There are no "deny" rules; you only ever add allows, and all rules are combined.
- **Stateful.** If a connection is allowed in, its response is allowed back out automatically, and vice versa. You never write rules for return traffic.
- **Attached to interfaces, not subnets.** Two resources in the same subnet can have completely different rules. Being "next to" something gives you nothing.
- **Can reference other security groups.** "Allow 5432 from `sg-app`" means "from any interface that carries `sg-app`", whatever its IP. This turns out to be the most important feature, and we'll get to it.

### What they are not

- **Not a subnet firewall.** That's a Network ACL: stateless, per subnet, supports deny rules. NACLs are a coarse backstop; security groups do the real work.
- **Not authentication.** A security group decides who can *open a connection*. It has no idea who the user is. The database password, IAM auth, and TLS are still your job.
- **Not a WAF.** Security groups work at layers 3 and 4: IPs, ports, protocols. They can't see an HTTP path, a SQL injection, or a malicious payload on an allowed port.
- **Not a log.** They don't record what they block or allow. That's VPC Flow Logs.
- **Not a fix for public-zone services.** S3 and DynamoDB don't sit behind your security groups. IAM protects them.

### Inbound vs. outbound

- **Inbound** rules decide who can start a conversation *with* this resource. This is what most people think about: "who can reach my database?"
- **Outbound** rules decide who this resource can start a conversation *with*. This is what most people forget.

AWS gives every new security group an outbound rule allowing **all traffic to anywhere**. That's convenient and it's a problem: if something inside is compromised, it can reach anything, including an attacker's server for exfiltration, or every other resource in your VPC.

Because security groups are stateful, **a database needs no outbound rules at all** for clients to query it. The responses go back on the connection the client opened. A database that only ever answers questions should never be asking any.

---

## The demonstration: one database, six steps

The setup: a Postgres RDS instance on port 5432, and an application (a Lambda or ECS service) that needs to query it. We'll start where too many projects start and work our way down.

### Level 0: public subnet, wide open

```
RDS: public subnet, PubliclyAccessible = true

Inbound:   ALL traffic   from 0.0.0.0/0
Outbound:  ALL traffic   to   0.0.0.0/0
```

**Who can reach the database:** everyone on the internet, on every port.

This is not hypothetical. Internet-wide scanners sweep every public IPv4 address continuously. A Postgres port open to the world is typically found within minutes and then hammered with password guesses and known exploits. The only thing between the internet and your data is one password, plus whatever bugs Postgres's pre-auth code path has this year.

Outbound wide open also means that if the instance is ever abused, nothing stops it reaching anywhere.

### Level 1: right port, still the whole internet

```
Inbound:   TCP 5432   from 0.0.0.0/0
Outbound:  (none)
```

Better: one port instead of all of them, and outbound is gone because, as above, a database doesn't need it. But the attack surface that matters, Postgres on the open internet, hasn't changed. This is the setup that trips AWS's own checks (Security Hub flags both "RDS instances should prohibit public access" and "security groups should not allow unrestricted access to high-risk ports").

### Level 2: scoped to one IP, still public

```
Inbound:   TCP 5432   from 203.0.113.10/32    (the office)
Outbound:  (none)
```

This is where many teams stop, because it *feels* locked down. It isn't:

- **The database is still on the internet.** It has a public DNS name and a public IP. One mis-edited rule, one "temporary" `0.0.0.0/0` for a contractor, one copied-and-pasted CIDR, and it's exposed again. The only wall is a single rule.
- **IPs aren't people.** That office IP is shared by everyone on the office network, and home or café IPs change and are shared with strangers. You're trusting a location, not an identity.
- **It doesn't solve the real access problem.** Your application, the thing that actually needs the database, probably doesn't have a stable public IP either.

#### Why a database should never be in a public subnet

This deserves to be blunt: **putting a database in a public subnet is one of the worst things you can do in AWS**, and no security group rule makes it acceptable.

- **It removes a layer of defence for no benefit.** In a private subnet there is no route from the internet. A rule mistake can't expose the database, because there's no path to expose. In a public subnet, your whole security posture rests on a rule set people edit by hand.
- **It fails the moment something else goes wrong.** Leaked credentials, a weak password, an unpatched Postgres CVE: each of these is survivable for a private database and catastrophic for a public one.
- **Nothing legitimate needs it.** Applications reach the database from inside the VPC. Humans who need direct access can use a bastion, SSM Session Manager port forwarding, or a VPN, all of which keep the database private.
- **Auditors and compliance frameworks will flag it immediately.** CIS, SOC 2 and PCI all treat a publicly accessible database as a finding.

If you take one thing from this post: databases go in private subnets. Always.

### Level 3: private subnet, VPC CIDR

We move the database into a private subnet, turn off public accessibility, and allow the whole VPC:

```
RDS: private subnet, PubliclyAccessible = false

Inbound:   TCP 5432   from 10.0.0.0/16    (the entire VPC)
Outbound:  (none)
```

This is a real improvement. The internet can no longer reach the database at all; there's no route. Rule mistakes now leak to your VPC, not the planet.

But look at what "from 10.0.0.0/16" means: **every resource in the VPC can connect to the database.** Your API, yes. Also the image-processing worker, the third-party log shipper, the forgotten test instance someone launched last year, and the container running a dependency with a fresh remote-code-execution CVE.

Security people call this the **blast radius**. A breach of any one resource in the CIDR range becomes a path to your data. An attacker who lands anywhere in the VPC, through a vulnerable library, an SSRF bug, or a misconfigured container, now has a direct line to Postgres. The network wall protects you from the outside; it does nothing about movement *inside* (lateral movement).

Narrowing to a subnet CIDR (`10.0.1.0/24`) shrinks the radius but doesn't change its nature: everything in that subnet, now and in the future, is trusted.

### Level 4: individual IPs

So allow only the application's addresses:

```
Inbound:   TCP 5432   from 10.0.1.23/32
           TCP 5432   from 10.0.1.47/32
           TCP 5432   from 10.0.2.12/32
           ...
```

On paper this is least privilege. In practice it falls apart:

- **Modern compute doesn't have stable IPs.** Lambda ENIs, ECS tasks, autoscaling groups and spot instances get new addresses as they scale, deploy and replace themselves. You'd be updating rules on every deploy.
- **Stale rules are dangerous, not just untidy.** When an instance goes away its IP goes back into the pool. The next thing to get `10.0.1.23` could be anything, and it inherits database access.
- **It doesn't scale.** A security group allows 60 inbound rules by default. Ten services across three AZs with autoscaling burns through that quickly.
- **Nobody can read it.** Six months from now, no one knows what `10.0.2.12` was.

The IP approach is right in spirit, *only the app*, and wrong in mechanism. IPs describe where something is. We want to describe what something is.

### Level 5: self-referencing security group

Security groups can name other security groups as a source, including themselves. Create one group, `db-access`, attach it to **both** the database and the application, and add one rule:

```
sg-db-access (attached to RDS and the app)

Inbound:   TCP 5432   from sg-db-access   (itself)
```

This reads as "anything wearing the `db-access` badge can reach port 5432 on anything else wearing it". Now:

- **Access follows identity, not address.** Scale the app to 200 tasks, redeploy, change subnets, and every interface carrying the group is allowed. No IPs to manage.
- **Removal is clean.** Take the group off a resource and its access ends; no stale IP rules waiting to be inherited.
- **A breach elsewhere in the VPC no longer reaches the database.** The image worker and the forgotten test instance don't carry the group, so they're dropped.

This is a big step, and it's where most well-run AWS accounts land. But it has a subtle flaw: **the trust goes both ways.** The rule applies to *every* member, so the database can open connections to the app on 5432, and every app that joins the group can reach every other member on 5432. Attach the group to one more service "because it needs the database" and it can now reach everything else in the group too. Membership is a club, and everyone in the club trusts everyone else.

### Level 6: one-way security group references

The fix is to stop sharing a group. Each resource gets **its own** security group, and rules point from one to the other in exactly one direction:

```
sg-app (attached only to the app)
  Inbound:   (none)
  Outbound:  TCP 5432   to   sg-db

sg-db (attached only to RDS)
  Inbound:   TCP 5432   from sg-app
  Outbound:  (none)
```

Read it out loud: *the app may open connections to the database on 5432, and the database accepts connections on 5432 from the app.* Nothing else.

- The database can't initiate anything. It has no outbound rules and doesn't need them.
- The app can reach the database and only the database. A compromised app can't use this rule to wander the VPC.
- A second service that needs the database gets its own rule pair, and gains no access to the first service.
- Each rule describes one relationship, so the security group *is* the architecture diagram.

That's least privilege at the network layer: the smallest set of connections that lets the system work, expressed in terms of what things are rather than where they happen to be.

| Level | Who can reach the DB on 5432 | Breach blast radius |
|---|---|---|
| 0. Public, all open | The internet, every port | Everything |
| 1. Public, 5432 open | The internet | Everything |
| 2. Public, one IP | That IP, and whoever is behind it | One rule away from everything |
| 3. Private, VPC CIDR | Every resource in the VPC | Any VPC resource → the DB |
| 4. Private, IP list | Whatever currently holds those IPs | Drifts as IPs are reused |
| 5. Self-referencing SG | Every member of the group, in both directions | Any member → all members |
| 6. One-way SG references | Exactly the app | The app, and only toward the DB |

---

## How Anvil does this

You shouldn't have to walk this ladder yourself every time, so Anvil starts at the top:

- **Every component gets its own security group, and it starts empty.** No inbound rules, and none of AWS's default allow-all outbound either. A new resource can talk to nothing until you say so.
- **Grants open the network and the permission together.** When you grant one resource access to another, Anvil writes the one-way rule pair from Level 6 *and* the IAM statement, so network access and permission are added and removed as one.

```ts
// App → DSQL on 5432: sg-fn egress to sg-dsql, sg-dsql ingress from sg-fn
dsql.grantConnect(fn, { schemas: [{ schema: 'my_app', roleName: 'app_role' }] });

// App → SQS through a VPC endpoint: 443 rule pair, IAM limited to that endpoint
queue.grantSendMessage(fn, { endpoint: sqsEndpoint });
```

Remove the grant and both the rules and the permission go with it. No shared groups, no CIDRs, no IP lists, and no "temporary" `0.0.0.0/0`.

If you genuinely need broad outbound access, for example a Lambda calling arbitrary third-party APIs through a NAT, there's an explicit, opt-in `allowAllOutbound` switch that warns at preview time. The wide-open option exists; it just isn't the default, and it's never silent.

---

## TL;DR

1. Databases go in private subnets. No exceptions.
2. Security groups are stateful: answering doesn't need outbound rules, so a database should have none.
3. CIDR rules trust everything in the range, including whatever gets breached next.
4. IP rules describe *where*, drift constantly, and get inherited by whatever reuses the IP.
5. Security group references describe *what*. Use one group per resource and one-way rules between them.
6. Anvil does 1, 2 and 5 by default: empty security groups, opened only by grants.
