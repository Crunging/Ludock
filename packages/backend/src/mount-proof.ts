import path from "node:path";
import { PassThrough } from "node:stream";
import type Docker from "dockerode";
import { docker } from "./docker-client.js";
import type { ContainerFileMount } from "./file-storage.js";
import { getHelperImage } from "./runtime-images.js";

export interface MountIdentity {
  dev: string;
  ino: string;
}
export interface MountProof {
  identities: Record<string, MountIdentity>;
  cleanup(): Promise<void>;
}

export const MOUNT_PROOF_SCRIPT = String.raw`
const fs=require("node:fs/promises"),C=require("node:fs").constants;
const sources=JSON.parse(process.argv[1]);const retained=[];
(async()=>{
 const identities={};
 for(const source of sources){
  if(!source.source.startsWith("/")||source.source.includes("\0")||source.source.split("/").includes(".."))throw new Error();
  let current=await fs.open("/host",C.O_RDONLY|C.O_DIRECTORY|C.O_NOFOLLOW);
  for(const part of source.source.split("/").filter(Boolean)){
   const next=await fs.open("/proc/self/fd/"+current.fd+"/"+part,C.O_RDONLY|C.O_DIRECTORY|C.O_NOFOLLOW);
   await current.close();current=next;
  }
  const info=await current.stat({bigint:true});retained.push(current);
  identities[source.destination]={dev:String(info.dev),ino:String(info.ino)};
 }
 process.stdout.write(JSON.stringify({identities})+"\n");setInterval(()=>{},3600000);
 if(Number(process.argv[2])>0)setTimeout(()=>process.exit(0),Number(process.argv[2]));
})().catch(()=>{process.stdout.write(JSON.stringify({error:"unsafe-source"})+"\n");process.exitCode=1;});
`;

function proofError(): Error {
  return Object.assign(
    new Error(
      "A data mount could not be verified. Bind source paths must contain no symbolic links and must be accessible to the Docker host validator; named volumes must use the local driver without host remapping options.",
    ),
    {
      code: "UNVERIFIED_DATA_MOUNT",
      statusCode: 409,
    },
  );
}

/** A private validator exposes only directory device/inode metadata. It never
 * runs user commands, reads game file contents, or exposes its host mount to a
 * file endpoint. Retaining its FDs prevents source inode reuse until cleanup. */
export async function createMountProof(
  mounts: readonly ContainerFileMount[],
  operationId?: string,
): Promise<MountProof> {
  for (const mount of mounts.filter((entry) => entry.Type === "volume")) {
    const name = mount.Name || mount.Source;
    if (!name || name.includes("/")) throw proofError();
    let info: Docker.VolumeInspectInfo;
    try {
      info = await docker.getVolume(name).inspect();
    } catch {
      throw proofError();
    }
    if (info.Driver !== "local" || Object.keys(info.Options || {}).length > 0)
      throw proofError();
  }
  const sources = mounts
    .filter((entry) => entry.Type === "bind")
    .map((mount) => ({
      source: path.posix.normalize(mount.Source).replace(/\/$/, ""),
      destination: path.posix.normalize(mount.Destination).replace(/\/$/, ""),
    }));
  if (!sources.length) return { identities: {}, cleanup: async () => {} };
  const image = getHelperImage();
  const options: Docker.ContainerCreateOptions = {
    Image: image,
    Entrypoint: ["bun", "-e"],
    Cmd: [
      MOUNT_PROOF_SCRIPT,
      JSON.stringify(sources),
      String(operationId ? 0 : 35 * 60_000),
    ],
    User: "0",
    Labels: {
      "ludock.enable": "false",
      "ludock.internal": "mount-validator",
      ...(operationId ? { "ludock.operation": operationId } : {}),
    },
    HostConfig: {
      Mounts: [
        {
          Type: "bind",
          Source: "/",
          Target: "/host",
          ReadOnly: true,
          BindOptions: {
            ReadOnlyForceRecursive: true,
          } as Docker.MountSettings["BindOptions"] & {
            ReadOnlyForceRecursive: boolean;
          },
        },
      ],
      NetworkMode: "none",
      ReadonlyRootfs: true,
      AutoRemove: true,
      CapDrop: ["ALL"],
      CapAdd: ["DAC_READ_SEARCH"],
      SecurityOpt: ["no-new-privileges"],
      PidsLimit: 32,
      Memory: 128 * 1024 * 1024,
      NanoCpus: 1_000_000_000,
    },
  };
  let container: Docker.Container;
  try {
    container = await docker.createContainer(options);
  } catch (error) {
    if ((error as { statusCode?: number }).statusCode !== 404)
      throw proofError();
    try {
      const input = await docker.pull(image);
      await new Promise<void>((resolve, reject) =>
        docker.modem.followProgress(input, (error) =>
          error ? reject(error) : resolve(),
        ),
      );
      container = await docker.createContainer(options);
    } catch {
      throw proofError();
    }
  }
  let removal: Promise<void> | undefined;
  const cleanup = () => {
    removal ??= container
      .remove({ force: true })
      .then(() => {})
      .catch((error: { statusCode?: number }) => {
        if (error.statusCode !== 404) throw proofError();
      });
    return removal;
  };
  try {
    await container.start();
    const stream = await container.logs({
      follow: true,
      stdout: true,
      stderr: false,
      tail: 10,
    });
    const output = new PassThrough();
    const ignored = new PassThrough();
    ignored.resume();
    const identities = await new Promise<Record<string, MountIdentity>>(
      (resolve, reject) => {
        let buffer = "";
        let finished = false;
        const timeout = setTimeout(() => finish(), 10_000);
        const finish = (result?: Record<string, MountIdentity>) => {
          if (finished) return;
          finished = true;
          clearTimeout(timeout);
          (stream as NodeJS.ReadableStream & { destroy(): void }).destroy();
          if (result) resolve(result);
          else reject(proofError());
        };
        output.on("data", (chunk: Buffer) => {
          buffer += chunk.toString("utf8");
          if (buffer.length > 16_384) {
            finish();
            return;
          }
          if (!buffer.includes("\n")) return;
          try {
            const parsed = JSON.parse(
              buffer.slice(0, buffer.indexOf("\n")),
            ) as { identities?: Record<string, MountIdentity> };
            const result = parsed.identities;
            if (
              !result ||
              sources.some(
                (source) =>
                  !result[source.destination] ||
                  !/^\d+$/.test(result[source.destination].dev) ||
                  !/^\d+$/.test(result[source.destination].ino),
              )
            )
              finish();
            else finish(result);
          } catch {
            finish();
          }
        });
        stream.once("error", () => finish());
        stream.once("end", () => finish());
        docker.modem.demuxStream(stream, output, ignored);
      },
    );
    return { identities, cleanup };
  } catch {
    await cleanup();
    throw proofError();
  }
}

/** For backup helpers that project roots under aliases. The caller must keep
 * the proof alive until the data helper is removed. */
export async function assertMountIdentities(
  container: Docker.Container,
  identities: Record<string, MountIdentity>,
): Promise<void> {
  if (!Object.keys(identities).length) return;
  const script = String.raw`
const fs=require("node:fs/promises"),C=require("node:fs").constants;
(async()=>{for(const [name,expected] of Object.entries(JSON.parse(process.argv[1]))){
 let file=await fs.open("/",C.O_RDONLY|C.O_DIRECTORY|C.O_NOFOLLOW);
 for(const part of name.split("/").filter(Boolean)){const next=await fs.open("/proc/self/fd/"+file.fd+"/"+part,C.O_RDONLY|C.O_DIRECTORY|C.O_NOFOLLOW);await file.close();file=next;}
 const info=await file.stat({bigint:true});await file.close();
 if(String(info.dev)!==expected.dev||String(info.ino)!==expected.ino)throw new Error();
}})().catch(()=>{process.exitCode=1});
`;
  const execution = await container.exec({
    Cmd: ["bun", "-e", script, JSON.stringify(identities)],
    AttachStdout: true,
    AttachStderr: true,
  });
  const stream = await execution.start({ hijack: true, stdin: false });
  stream.resume();
  await new Promise<void>((resolve, reject) => {
    const timeout = setTimeout(() => {
      stream.destroy();
      reject(proofError());
    }, 10_000);
    stream.once("end", () => {
      clearTimeout(timeout);
      resolve();
    });
    stream.once("error", () => {
      clearTimeout(timeout);
      reject(proofError());
    });
  });
  if ((await execution.inspect()).ExitCode !== 0) throw proofError();
}
