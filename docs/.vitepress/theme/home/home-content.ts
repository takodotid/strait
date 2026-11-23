// What the home page says. Each line is taken from a page of the docs, which stays the authority: the path and the
// command from guide/introduction.md, the route marks from reference/cli.md, the links from guide/concepts.md.

export const REPOSITORY = "https://github.com/takodotid/strait";

export const NAME = "pg.app.svc:5432";

export const EXPLAIN_COMMAND = "strait --context site-b explain pg -n app";

/** The three moments the drawing goes through: the primary at site a, the sites disagreeing, the primary at site b. */
export const MOMENTS = [
    {
        primary: "a",
        routed: true,
        short: "The primary runs at site a",
        line: "The primary runs at site a. At site b the name points at the gateway, which carries each connection over the link.",
    },
    {
        primary: "",
        routed: false,
        short: "The sites declare different primaries",
        line: "The sites declare different primaries. The name has no endpoints, so a new connection is refused at once and the client retries.",
    },
    {
        primary: "b",
        routed: true,
        short: "The primary runs at site b",
        line: "The primary runs at site b. The name follows it, and the application holds no topology.",
    },
] as const;

export const PATH = [
    { name: "The application connects", does: "It connects to pg.app.svc, the same name at every site. It holds no host lists and no failover logic.", where: "Site b" },
    { name: "The name points at the gateway", does: "The primary is remote, so the controller at site b wrote the gateway as the name's endpoints.", where: "Site b" },
    { name: "The gateway carries it across", does: "Each connection goes over the link to the gateway at site a: a direct path, or a WireGuard tunnel.", where: "Link" },
    { name: "It reaches the primary", does: "The gateway at site a passes the connection on to pg-a.", where: "Site a" },
];

export const ROUTES = [
    { mark: "confirmed", name: "A confirmed member", when: "The name points at a member whose state the site has confirmed." },
    { mark: "kept", name: "A route kept", when: "A member is Unknown, or the controller awaits its peers." },
    { mark: "nowhere", name: "Nowhere", when: "Two members are writable, the sites declare different primaries, or a member stays Unknown too long." },
] as const;

/** Said once for both transports, after the table of links in guide/concepts.md. */
export const LINK_FAILOVER = "When the active link fails, new connections move to the next one within 5 seconds.";

export const LINKS: { name: string; flag: string; is: string; note?: string }[] = [
    {
        name: "Direct",
        flag: "transport: direct",
        is: "A private path with no tunnel, declared encryption: None.",
        note: "Where pod addresses are routable between sites, the name can point straight at the remote pods, with no gateway hop.",
    },
    {
        name: "WireGuard",
        flag: "transport: wireguard",
        is: "A kernel WireGuard tunnel between the gateways.",
    },
];
