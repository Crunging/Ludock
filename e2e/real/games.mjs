// The games real/rig.mjs can run, each as an owner would define it under
// Compose. service is the Compose service; the rig publishes its ports on
// 127.0.0.1 only and mounts the rig's <game>/data folder at data, if set. ready is a
// log line that says the game takes players, or "healthy" for an image with a
// health check. licence names an environment variable the owner sets to TRUE
// to accept the game's terms for the test servers.
export const games = {
  minecraft: {
    licence: { env: "MINECRAFT_EULA", terms: "https://aka.ms/MinecraftEULA" },
    data: "/data",
    ready: "healthy",
    service: {
      image: process.env.LUDOCK_RIG_JAVA_IMAGE || "itzg/minecraft-server:java21",
      environment: {
        EULA: "TRUE",
        // Mineflayer supports this release; the bot can't join a newer one.
        VERSION: process.env.LUDOCK_RIG_MINECRAFT_VERSION || "1.21.11",
        MEMORY: "1G", VIEW_DISTANCE: "4", SIMULATION_DISTANCE: "4", ONLINE_MODE: "FALSE", MOTD: "Ludock real rig",
      },
      mem_limit: "2g",
      ports: ["25565:25565"],
    },
  },
  terraria: {
    data: "/root/.local/share/Terraria/Worlds",
    ready: /Server started/,
    service: {
      image: "hexlo/terraria-server-docker:latest",
      environment: { worldname: "rig", autocreate: "1", upnp: "0" },
      stdin_open: true,
      tty: true,
      mem_limit: "1536m",
      ports: ["7777:7777"],
    },
  },
  factorio: {
    data: "/factorio",
    ready: "healthy",
    service: {
      image: "factoriotools/factorio:stable",
      mem_limit: "1536m",
      ports: ["34197:34197/udp", "27015:27015/tcp"],
    },
  },
  valheim: {
    // The image's README mounts the server's own files too.
    data: "/config",
    ready: /Game server connected/,
    service: {
      image: "lloesche/valheim-server:latest",
      environment: { SERVER_NAME: "Ludock real rig", WORLD_NAME: "rig", SERVER_PASS: "rig-password", SERVER_PUBLIC: "false" },
      mem_limit: "4g",
      ports: ["2456-2457:2456-2457/udp"],
      volumes: ["server:/opt/valheim"],
      stop_grace_period: "2m",
    },
  },
  "vintage-story": {
    data: "/vintagestory/data",
    ready: /Dedicated Server now running/,
    service: {
      image: "zsuatem/vintagestory:latest",
      stdin_open: true,
      tty: true,
      mem_limit: "2g",
      ports: ["42420:42420/tcp"],
    },
  },
  "minecraft-bedrock": {
    licence: { env: "MINECRAFT_EULA", terms: "https://aka.ms/MinecraftEULA" },
    data: "/data",
    ready: "healthy",
    service: {
      image: "itzg/minecraft-bedrock-server:latest",
      environment: { EULA: "TRUE", SERVER_NAME: "Ludock real rig" },
      stdin_open: true,
      tty: true,
      mem_limit: "1g",
      ports: ["19132:19132/tcp", "19132:19132/udp"],
    },
  },
  palworld: {
    ready: /Running Palworld dedicated server/,
    timeout: 1_800_000,
    service: {
      image: "thijsvanloef/palworld-server-docker:latest",
      environment: { RCON_ENABLED: "true", ADMIN_PASSWORD: "rig-admin-password", PLAYERS: "4", COMMUNITY: "false",
        SERVER_NAME: "Ludock real rig", BACKUP_ENABLED: "false" },
      mem_limit: "4g",
      ports: ["8211:8211/udp", "27015:27015/udp"],
      // The saves live inside the server the image downloads.
      volumes: ["palworld:/palworld"],
    },
  },
};
