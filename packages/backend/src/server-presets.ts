export interface ServerImagePreset {
  gameType: string;
}

const IMAGE_PRESETS: ReadonlyArray<{
  repositories: readonly string[];
  preset: ServerImagePreset;
}> = [
  {
    repositories: ["itzg/minecraft-server"],
    preset: { gameType: "minecraft" },
  },
  {
    repositories: ["factoriotools/factorio"],
    preset: { gameType: "factorio" },
  },
  {
    repositories: [
      "thijsvanloef/palworld-server-docker",
      "jammsen/palworld-dedicated-server",
    ],
    preset: { gameType: "palworld" },
  },
  {
    repositories: [
      "hermsi/ark-server",
      "ghcr.io/hermsi1337/ark-server",
      "quay.io/hermsi1337/ark-server",
      "indifferentbroccoli/ark-server-docker",
    ],
    preset: { gameType: "ark-survival-evolved" },
  },
  {
    repositories: [
      "sknnr/ark-ascended-server",
      "mschnitzer/asa-linux-server",
    ],
    preset: { gameType: "ark-survival-ascended" },
  },
  {
    repositories: ["joedwards32/cs2"],
    preset: { gameType: "cs2" },
  },
  {
    repositories: [
      "renegademaster/zomboid-dedicated-server",
      "ghcr.io/renegade-master/zomboid-dedicated-server",
      "quay.io/renegade_master/zomboid-dedicated-server",
    ],
    preset: { gameType: "project-zomboid" },
  },
  {
    repositories: [
      "indifferentbroccoli/conan-exiles-enhanced-server-docker",
    ],
    preset: { gameType: "conan-exiles" },
  },
  {
    repositories: ["trueosiris/vrising"],
    preset: { gameType: "v-rising" },
  },
  {
    repositories: ["didstopia/rust-server"],
    preset: { gameType: "rust" },
  },
  {
    repositories: ["vinanrra/7dtd-server"],
    preset: { gameType: "7-days-to-die" },
  },
  {
    repositories: [
      "ghcr.io/community-valheim-tools/valheim-server",
      "ghcr.io/lloesche/valheim-server",
      "lloesche/valheim-server",
    ],
    preset: { gameType: "valheim" },
  },
  {
    repositories: [
      "hexlo/terraria-server-docker",
      "beardedio/terraria",
      "ghcr.io/beardedio/terraria",
      "ryshe/terraria",
    ],
    preset: { gameType: "terraria" },
  },
];

export function getServerImagePreset(
  image: string
): ServerImagePreset | undefined {
  const repository = normalizeImageRepository(image);
  return IMAGE_PRESETS.find(({ repositories }) =>
    repositories.includes(repository)
  )?.preset;
}

export function inferGameType(image: string): string {
  return getServerImagePreset(image)?.gameType || "unknown";
}

function normalizeImageRepository(image: string): string {
  let repository = image.trim().toLowerCase().split("@", 1)[0] || "";
  const lastSlash = repository.lastIndexOf("/");
  const lastColon = repository.lastIndexOf(":");
  if (lastColon > lastSlash) repository = repository.slice(0, lastColon);

  if (repository.startsWith("index.docker.io/")) {
    repository = repository.slice("index.docker.io/".length);
  } else if (repository.startsWith("docker.io/")) {
    repository = repository.slice("docker.io/".length);
  }

  return repository;
}
