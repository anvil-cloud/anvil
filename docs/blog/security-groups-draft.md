# Security groups, from 0.0.0.0/0 to least privilege

> Draft. Audience: developers who deploy to AWS but haven't had to think hard about networking yet.

Security groups are the most-used and least-understood control in AWS. Every VPC resource has one, almost every tutorial opens one too wide, and the console makes "allow everything" a two-click affair. This post walks one database from the worst possible setup to the one Anvil builds for you, one step at a time, and explains what each step actually buys.

First, the groundwork: you can't reason about a security group without knowing what network it sits in.

## The groundwork

### What is a Virtual Private Cloud?

A Virtual Private Cloud (VPC) is a private network inside your AWS region. You select a CIDR block for your address range (basically an IP address range allocated for that VPC). Nothing gets in or out unless you allow it. Some of your AWS resources will live there, and I'll cover how they communicate with each other within that VPC in this article.

### The AWS Public Zone

The AWS Public Zone holds services managed by AWS. You can reach them over HTTPS on the public internet, but IAM guards them. Not every managed resource lives here; for example, RDS runs on instances inside your VPC, so you reach it over your network rather than a public endpoint. A DynamoDB table, by contrast, has a public endpoint.

The credentials making the call need permission to use the service. If you are logged into a role that can send SQS messages, you can send one; if you try to put an item into DynamoDB without permissions, it will be denied.

If an attacker steals credentials, they can call whatever the permissions allow. If you have a DynamoDB table with data on it and your role has the ability to read that table. They have just run off with your data. That's why least privilege is important. A role that doesn't need access to a service shouldn't have it. Reduce your blast radius before it's too late.

### Subnets: public vs. private

They vary in access and functionality. A subnet stays private unless you explicitly make it public; to do that, attach an Internet Gateway (IGW) to the VPC it's in and add a route to the IGW in the route table. An IGW is a two-way gateway that allows inbound and outbound traffic. A route that sends 0.0.0.0/0 to the IGW catches any traffic headed outside the VPC that no other route handles. Other routes, like local, provide a path between subnets in the VPC.

A path is not explicit permission. When traffic leaves the subnet to communicate with another subnet, it might not actually have access to anything.

Private subnets that require outbound traffic will use a NAT Gateway. It only allows outbound traffic from the private subnet. It's important to understand that it will only allow inbound traffic back from whatever is sent outbound. You cannot initiate inbound traffic on a NAT Gateway.

The route table associated with the private subnet has a 0.0.0.0/0 route that points to the NAT Gateway in a public subnet, which then sends traffic out via the IGW. Remember, a subnet is only public if its VPC has an IGW attached and its route table sends traffic to it.

Simply put, a VPC can have an IGW attached, and a route table provides a path for inbound and outbound traffic. A NAT Gateway sits in a public subnet; traffic from a private subnet routes out via the NAT Gateway and on through the IGW.

## Security groups

So now we understand how the traffic enters a VPC and flows between subnets within that VPC. When you have resources, how do you control what traffic reaches those resources? Remember, just because traffic can reach a subnet doesn't mean it has permission to do anything with the resources inside the VPC and subnets.

A security group is a stateful firewall, which means that it keeps state on inbound and outbound traffic. If security group rules allow outbound traffic, it will allow the inbound response. It applies on the inverse as well: if the security group rules allow inbound traffic, it can send an outbound response back to that same call.

Security groups will attach to your resources inside the VPC and allow traffic based on inbound and outbound rules. They don't have deny rules; they're explicit allow-only. Anything that doesn't match the rules for that resource will get dropped.

## The demonstration: resources that progressively get more strict.

The setup: a Postgres RDS instance on port 5432, and an application (a Lambda or ECS service) that needs to query it. We'll start where too many projects start and work our way down. For this example, we'll focus on traffic inside the VPC and subnets, not how it gets there.

I am not a huge fan of demonstrations that show you one way and one way only. I think it's more beneficial to understand why something is bad and why others are acceptable.

### Public subnets

First, please don't put your database in a public subnet. You can have an RDS in a public subnet that no traffic can reach because the security groups have no inbound rules. Meaning that no traffic can reach it at all. It's essentially useless.

Even if you did scope it down to something you think is safe, it's going to get hammered with automated scanners. This is the worst thing you could possibly do to your database. It still needs credentials to access it, but this is basically inviting anyone to breach your database. It's one misconfigured security group away from being exposed.

RDS usually doesn't need outbound rules at all. It only answers requests, and because security groups are stateful, the replies go back automatically. The same goes for anything whose only job is to respond.

The same applies to anything in the public subnet with security groups. Outbound is easy to overlook because AWS gives every new security group an "allow all traffic to 0.0.0.0/0" rule by default. If an attacker gets remote code execution on one of your servers, nothing stops it from sending your data out or mining Bitcoin on your bill.

### Moving to a private subnet

So you decided, after getting attacked on a public subnet, that it's a horrendous idea and moved your database into a private subnet with an inbound rule that allows TCP on all ports, with your VPC CIDR block as the source range.

Is it better? Yes. This is what I normally hear: "at least it's better than…" when, in reality, when designing your architecture and system, you should be asking, "What is the impact if something does go wrong?"

The blast radius is still big. This means that everything inside the VPC can access your RDS instance. Even resources in another subnet. It's not open to the public, and anything sending traffic in would need an explicit path.

After you ask yourself the number one question, "What is the impact if something does go wrong?", you notice the blast radius is big, and your database instance is reachable on all ports. So you scope inbound to your app subnet's CIDR block and allow TCP only on port 5432. All ports aren't necessarily dangerous on an RDS instance because it only listens on a certain port. Don't open ports you don't need; the way you do one thing is the way you do everything.

The blast radius is smaller; is it better? Yes. But it's the same problem as above with a smaller blast radius. Anything in the app subnet can reach it, even resources that have no business accessing the database.

### Allowlisting individual IPs

Now you want to reduce the blast radius and start thinking about least-privilege access, which means you are in the right frame of mind. You are now thinking about individual resources and how your system actually works, instead of it working because of broad permissions and not knowing why.

So you start scoping inbound down to IP addresses. Then you realise this isn't scalable and adds operational overhead, and some resources don't even have static IPs. Lambdas and ECS tasks, for example, don't have them, and they are a big part of compute in applications running on AWS.

Security-wise, it's not the worst, but I wouldn't recommend it. When a resource's IP is released, it goes back into the subnet pool, meaning a new, unrelated resource could inherit access to your database when the IP is reallocated. In ECS Fargate, every task gets a fresh IP when it starts, so addresses are constantly being released and reused. A task with no reason to touch your database can pick up an IP your security group rules still trust, and it gains access.

"What is the impact if something does go wrong?" Probably less than before, but the operational overhead isn't worth the strain. It's a nightmare to maintain, and it is a sure way to create deployment bottlenecks; a simple feature becomes a 3-month project with an unreal amount of pointless meetings.

### Self-referencing security groups

If you and your team cannot handle the operational overhead and risk of static IPs for security group rules. The next step is to look into self-referencing security groups.

Self-referencing security groups solve the problem with IP rules: create a group called rds-sg-access with inbound rules that allow TCP connections on port 5432 from anything in the rds-sg-access group and then an outbound rule that allows the same.

Attach this to the RDS instance and any other resources that need access. Anything in the group can now reach anything else in it on 5432. Your Lambda can reach the database, and you don't need any IP rules.

To remove access, remove the group from the resource and communication stops. Each resource can carry up to 5 security groups. For most resources, that's a soft limit you can raise to 16, but Lambda and ECS cap it at 5 regardless. Every shared group like this uses one of those slots.

Is it better? Yes. Is it less operational overhead? Yes.

But "What is the impact if something does go wrong?"

If one resource in the group is compromised, it can reach every member on port 5432, not just the database that the lambda needs. Because the group allows TCP on 5432, every member is exposed on that port to every other member. For some resources, this doesn't matter. A Lambda doesn't listen on 5432, so being reachable on it changes nothing. But anything that does listen on 5432, like a second database, is exposed to every member.

The blast radius is significantly smaller than before. Only resources explicitly attached to the group are affected; things in the same VPC or subnet without the group can't reach the database.

If you have groups with wider rules, the problem grows with the number of resources, and it becomes harder to understand what has access to what.

Obviously, no breach is good, but knowing that the blast radius is smaller is a massive improvement over everything being affected. For a business or some people, this level of security would be acceptable. I don't blame them: it's scalable, and it's explicit. Group membership tells you exactly which resources should be accessible.

### One-way security group references

Each resource gets its own group, and each rule names the other group in one direction only. In the section above, the lambda and the database shared the same rds-sg-access group. The group is now split into two.

```
lambda-sg   Inbound:  none
            Outbound: TCP 5432 to rds-sg

rds-sg      Inbound:  TCP 5432 from lambda-sg
            Outbound: none
```

Only the resources you've granted can open connections to the database, and the database accepts connections from them alone. It doesn't start connections of its own.

Is it better? Yes. Is it less operational overhead? No. Every relationship now needs a rule on each side.

But "What is the impact if something does go wrong?"

A compromised resource can only reach what you explicitly granted. The blast radius is tiny, but the impact could still be huge: a compromised Lambda can reach the database. Security group rules limit how far a breach can go; the database's own permissions limit what it can do once it gets there.

The compromised lambda can only reach the database, and that's it. Add a second database, and it gets its own group.

```
lambda-sg   Inbound:  none
            Outbound: TCP 5432 to rds-sg

rds-sg      Inbound:  TCP 5432 from lambda-sg      ✅ Lambda → database
            Outbound: none

rds2-sg     Inbound:  none                         ❌ Lambda → second database
            Outbound: none
```

The Lambda can't reach it until you add a rule on both sides.

```
lambda-sg   Outbound: TCP 5432 to rds-sg
                      TCP 5432 to rds2-sg          ← new

rds2-sg     Inbound:  TCP 5432 from lambda-sg      ← new
```

This is least privilege at the network layer. Each rule describes one relationship, so your security groups describe your architecture. If you can't explain why a rule exists, then you don't need it.

### The catch

Every relationship now needs two rules: one on each side. The caller needs permission to send, and the target needs permission to accept. The more resources you add, the more rules you write, review, and keep in sync. It's still stateful, so you're not writing rules for replies, but you are writing both ends of every connection.

There are limits too. A security group can have 60 inbound and 60 outbound rules by default. With one-way references, every caller adds an inbound rule to the target, so a database or endpoint that everything talks to hits 60 first. You can raise it to 200 without touching anything else, but quota increases apply to your whole account in that region. The upside: each resource carries only its own group, so the 5-group cap that hurt self-referencing groups doesn't come into play.

```
rds-sg      Inbound:  TCP 5432 from lambda-1-sg
                      TCP 5432 from lambda-2-sg
                      TCP 5432 from lambda-3-sg
                      ...
                      TCP 5432 from lambda-60-sg    ← default limit: 60 inbound rules
                      TCP 5432 from lambda-61-sg    ❌ fails until the quota is raised
            Outbound: none

lambda-1-sg Inbound:  none
            Outbound: TCP 5432 to rds-sg            ← 1 of 60 outbound rules
                      TCP 5432 to rds2-sg           ← 2 of 60
                      TCP 443 to secrets-vpce-sg    ← 3 of 60
                      ...
                      TCP 5432 to rds61-sg ← 61 of 60 ❌ fails until the quota is raised

lambda-1    Security groups: lambda-1-sg            ← always 1 of 5 slots used
```

Operationally, this is a lot to track by hand, and hitting limits with large serverless apps is a real possibility. That might not be a trade-off some places are willing to make, and I think that's fair.

## This is why I built Anvil Cloud

Anvil takes care of all of this for you. Everything is explicitly stated in your infrastructure-as-code framework.

Every resource gets its own security group, and it starts empty: no inbound, no outbound, not even AWS's default. Nothing can talk to anything until you grant it:

```ts
const vpc = new anvil.aws.Vpc('core');
const net = { vpcId: vpc.vpcId, privateSubnetIds: vpc.privateSubnetIds };

const db = new anvil.aws.Rds('orders-db', {
  engine: 'postgres',
  vpc: net,
});

const fn = new anvil.aws.Lambda('orders-api', {
  runtime: 'nodejs24.x',
  handler: 'src/index.handler',
  vpc: net,
});

// Outbound 5432 on orders-api's group, inbound 5432 on orders-db's group.
db.grantConnect(fn);
```

As your application grows, Anvil handles it all for you. If you remove the resources and the grants, the security groups get deleted.

After everything we've just been through, every level, every trade-off, every "it depends on your tolerance for risk", this almost feels anticlimactic. A handful of lines, and you have least privilege at the network layer with no operational overhead.
