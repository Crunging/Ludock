interface IpAddress {
  address: string;
  value: bigint;
  bits: 32 | 128;
}

const IPV4_MAPPING = 0xffff00000000n;

/** Strict literals only: URL parsing validates and canonicalizes IPv6, while
 * IPv4 deliberately excludes URL shorthand, octal addresses, and ports. */
function parseIp(value: string): IpAddress | null {
  if (/^(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2})){3}$/.test(value)) {
    const octets = value.split(".").map(Number);
    if (octets.some(octet => octet > 255)) return null;
    const number = octets.reduce((result, octet) => (result << 8n) | BigInt(octet), 0n);
    return { address: value, value: IPV4_MAPPING | number, bits: 32 };
  }
  if (!value.includes(":") || !/^[0-9a-f:.]+$/i.test(value)) return null;
  let address: string;
  try { address = new URL(`http://[${value}]/`).hostname.slice(1, -1); }
  catch { return null; }
  const [left, right] = address.split("::");
  const leading = left ? left.split(":") : [];
  const trailing = right ? right.split(":") : [];
  const groups = right === undefined ? leading : [
    ...leading, ...Array.from({ length: 8 - leading.length - trailing.length }, () => "0"), ...trailing,
  ];
  const number = groups.reduce((result, group) => (result << 16n) | BigInt(`0x${group}`), 0n);
  // Treat an IPv4 peer and its IPv4-mapped IPv6 representation as one source.
  if (number >> 32n === 0xffffn)
    address = [24n, 16n, 8n, 0n].map(shift => Number((number >> shift) & 255n)).join(".");
  return { address, value: number, bits: 128 };
}

/** Only a configured proxy may supply the next hop. Walking right to left
 * stops before client-controlled prefixes in an appended forwarding chain. */
export function createClientIpResolver(configured = process.env.LUDOCK_TRUSTED_PROXIES || "") {
  const ranges = configured.trim() ? configured.split(",").map(entry => {
    const [literal, prefix, extra] = entry.trim().split("/");
    const ip = parseIp(literal);
    if (!ip || extra !== undefined ||
      (prefix !== undefined && (!/^(0|[1-9]\d{0,2})$/.test(prefix) || Number(prefix) > ip.bits)))
      throw new Error("LUDOCK_TRUSTED_PROXIES must contain comma-separated IP addresses or CIDRs");
    const length = prefix === undefined ? ip.bits : Number(prefix);
    const shift = BigInt(ip.bits - length);
    return { network: ip.value >> shift, shift };
  }) : [];
  const trusted = (ip: IpAddress) => ranges.some(range => ip.value >> range.shift === range.network);

  return (request: Pick<Request, "headers">, peerAddress: string | undefined): string | undefined => {
    const peer = peerAddress ? parseIp(peerAddress) : null;
    if (!peer) return peerAddress;
    if (!trusted(peer)) return peer.address;
    const header = request.headers.get("x-forwarded-for");
    if (!header) return peer.address;
    const hops = header.split(",");
    if (hops.length > 32) return peer.address;
    let current = peer;
    for (let index = hops.length - 1; index >= 0 && trusted(current); index--) {
      const next = parseIp(hops[index].trim());
      // Malformed trusted metadata cannot select an attacker-chosen source.
      if (!next) return peer.address;
      current = next;
    }
    return current.address;
  };
}
