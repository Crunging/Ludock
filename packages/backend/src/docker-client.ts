import { JsonLineDecoder } from "./json-lines.js";
import type { DockerConnection } from "./docker-transport.js";
export type { DockerConnection } from "./docker-transport.js";
import { dockerContainerIdSchema } from "@ludock/shared";
import { DockerApiError, DockerTransport } from "./docker-transport.js";
import type {
  ContainerAttachOptions, ContainerCreateOptions, ContainerInfo,
  ContainerInspectInfo, ContainerLogsOptions, ContainerStats, DockerFilters,
  ExecCreateOptions, ExecInspectInfo, VolumeInspectInfo,
} from "./docker-types.js";

export type * from "./docker-types.js";

// The tested Engine API ceiling. Recursive read-only bind proofs need v1.44.
// Negotiate once per client, sharing concurrent discovery requests.
const MAX_API_MINOR = 55;
const MIN_API_MINOR = 44;

function query(path: string, values: object): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(values) as Array<[string, unknown]>) {
    if (value === undefined) continue;
    const encoded = typeof value === "string" ? value : JSON.stringify(value);
    if (encoded === undefined) throw new Error("Invalid Docker query parameter");
    params.set(key, encoded);
  }
  return params.size ? `${path}?${params.toString()}` : path;
}

function resource(value: string): string {
  if (!/^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/.test(value))
    throw new Error("Invalid Docker resource identifier");
  return encodeURIComponent(value);
}

function apiMinor(value: unknown): number {
  if (typeof value !== "string" || !/^1\.\d+$/.test(value))
    throw new Error("Docker returned an invalid API version");
  return Number(value.slice(2));
}

export class DockerClient {
  readonly transport: DockerTransport;
  private version: Promise<string> | undefined;

  constructor(options: { socketPath: string }) {
    this.transport = new DockerTransport(options.socketPath);
  }

  private negotiate(): Promise<string> {
    this.version ??= this.transport.json<{ ApiVersion: string; MinAPIVersion?: string }>("/version")
      .then((version) => {
        const minor = Math.min(MAX_API_MINOR, apiMinor(version.ApiVersion));
        if (minor < MIN_API_MINOR || (version.MinAPIVersion && minor < apiMinor(version.MinAPIVersion)))
          throw new Error("Docker Engine has no supported API version for this client");
        return `v1.${minor}`;
      }).catch((error: unknown) => {
        this.version = undefined;
        throw error;
      });
    return this.version;
  }

  async path(path: string): Promise<string> {
    return `/${await this.negotiate()}${path}`;
  }

  async ping(): Promise<void> {
    const response = await this.transport.request("/_ping");
    if ((await response.text()).trim() !== "OK") throw new Error("Invalid Docker ping response");
    await this.negotiate();
  }

  async listContainers(options: { all?: boolean; filters?: DockerFilters } = {}): Promise<ContainerInfo[]> {
    return this.transport.json(await this.path(query("/containers/json", options)));
  }

  getContainer(id: string): Container {
    return new Container(this, id);
  }

  async createContainer(options: ContainerCreateOptions): Promise<Container> {
    const { name, ...body } = options;
    const result = await this.transport.json<{ Id: string }>(
      await this.path(query("/containers/create", { name })), "POST", body,
    );
    return this.getContainer(result.Id);
  }

  getVolume(name: string): Volume {
    return new Volume(this, name);
  }

  getImage(reference: string): { inspect(): Promise<{ Id: string }> } {
    // Image references contain registry ports, slashes, and digests. Encode the
    // entire segment so they cannot become query parameters or another endpoint.
    if (!reference || reference === "." || reference === "..") throw new Error("Invalid Docker image reference");
    const endpoint = `/images/${encodeURIComponent(reference)}/json`;
    return { inspect: async () => this.transport.json(await this.path(endpoint)) };
  }

  async createVolume(options: { Name: string }): Promise<Volume> {
    const result = await this.transport.json<VolumeInspectInfo>(await this.path("/volumes/create"), "POST", options);
    return this.getVolume(result.Name);
  }

  async getEvents(options: { filters?: DockerFilters } = {}): Promise<ReadableStream<Uint8Array>> {
    return this.transport.stream(await this.path(query("/events", options)));
  }

  /** Resolve only after every progress record has been checked. HTTP 200 alone
   * does not mean a pull succeeded; Docker reports registry errors in the body. */
  async pull(image: string): Promise<void> {
    const response = await this.transport.request(
      await this.path(query("/images/create", { fromImage: image })), "POST",
    );
    if (!response.body) throw new Error("Docker returned an empty pull response");
    const reader = (response.body as ReadableStream<Uint8Array>).getReader();
    let records = 0;
    const decoder = new JsonLineDecoder(record => {
      if (!record || typeof record !== "object" || Array.isArray(record))
        throw new Error("Docker returned invalid pull progress");
      if ("error" in record || "errorDetail" in record) throw new DockerApiError(500);
      records++;
    });
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        decoder.push(value);
      }
      decoder.end();
      if (!records) throw new Error("Docker returned an empty pull response");
    } catch (error) {
      if (error instanceof DockerApiError) throw error;
      // oxlint-disable-next-line preserve-caught-error -- Parser diagnostics may include daemon data.
      throw new Error("Docker returned invalid or oversized pull progress");
    } finally {
      await reader.cancel();
      reader.releaseLock();
    }
  }
}

export class Container {
  readonly id;
  private readonly endpoint: string;

  constructor(private readonly client: DockerClient, id: string) {
    const parsed = dockerContainerIdSchema.safeParse(id);
    if (!parsed.success) throw new Error("Invalid Docker container identifier");
    this.id = parsed.data;
    this.endpoint = `/containers/${resource(id)}`;
  }

  async inspect(): Promise<ContainerInspectInfo> {
    return this.client.transport.json(await this.client.path(`${this.endpoint}/json`));
  }

  async stats(options: { stream: false }): Promise<ContainerStats> {
    return this.client.transport.json(await this.client.path(query(`${this.endpoint}/stats`, options)));
  }

  async start(): Promise<void> {
    await this.client.transport.empty(await this.client.path(`${this.endpoint}/start`), "POST");
  }

  async stop(options: { t?: number } = {}): Promise<void> {
    await this.client.transport.empty(await this.client.path(query(`${this.endpoint}/stop`, options)), "POST");
  }

  async restart(): Promise<void> {
    await this.client.transport.empty(await this.client.path(`${this.endpoint}/restart`), "POST");
  }

  async remove(options: { force?: boolean; v?: boolean } = {}): Promise<void> {
    await this.client.transport.empty(await this.client.path(query(this.endpoint, options)), "DELETE");
  }

  async wait(): Promise<{ StatusCode: number }> {
    return this.client.transport.json(await this.client.path(`${this.endpoint}/wait`), "POST");
  }

  async logs(options: ContainerLogsOptions): Promise<ReadableStream<Uint8Array>> {
    return this.client.transport.stream(await this.client.path(query(`${this.endpoint}/logs`, options)));
  }

  async exec(options: ExecCreateOptions): Promise<Exec> {
    const result = await this.client.transport.json<{ Id: string }>(
      await this.client.path(`${this.endpoint}/exec`), "POST", options,
    );
    return new Exec(this.client, result.Id);
  }

  async attach(options: ContainerAttachOptions): Promise<DockerConnection> {
    return this.client.transport.hijack(await this.client.path(query(`${this.endpoint}/attach`, options)));
  }
}

export class Exec {
  private readonly endpoint: string;

  constructor(private readonly client: DockerClient, readonly id: string) {
    this.endpoint = `/exec/${resource(id)}`;
  }

  async start(): Promise<DockerConnection> {
    return this.client.transport.hijack(await this.client.path(`${this.endpoint}/start`), { Detach: false, Tty: false });
  }

  async inspect(): Promise<ExecInspectInfo> {
    return this.client.transport.json(await this.client.path(`${this.endpoint}/json`));
  }
}

export class Volume {
  private readonly endpoint: string;

  constructor(private readonly client: DockerClient, readonly name: string) {
    this.endpoint = `/volumes/${resource(name)}`;
  }

  async inspect(): Promise<VolumeInspectInfo> {
    return this.client.transport.json(await this.client.path(this.endpoint));
  }

  async remove(): Promise<void> {
    await this.client.transport.empty(await this.client.path(this.endpoint), "DELETE");
  }
}

export const docker = new DockerClient({
  socketPath: process.env.DOCKER_SOCKET || "/var/run/docker.sock",
});
