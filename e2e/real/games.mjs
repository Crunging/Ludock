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
  "core-keeper": {
    // As the image's README mounts them: the world, and the server download.
    data: "/home/steam/core-keeper-data",
    ready: /Started session with info/,
    service: {
      image: "escaping/core-keeper-dedicated:latest",
      environment: { WORLD_NAME: "Ludock real rig" },
      mem_limit: "5g",
      // Steam's relay by default, with no port to publish.
      ports: [],
      volumes: ["server:/home/steam/core-keeper-dedicated"],
      stop_grace_period: "2m",
    },
  },
  "v-rising": {
    data: "/mnt/vrising/persistentdata",
    ready: /Server Setup Complete|Startup Completed/,
    timeout: 1_800_000,
    service: {
      image: "trueosiris/vrising:latest",
      // The image writes HOST_SETTINGS_ variables into ServerHostSettings.json.
      environment: { SERVERNAME: "Ludock real rig", WINEDEBUG: "fixme-all",
        HOST_SETTINGS_Rcon__Enabled: "true", HOST_SETTINGS_Rcon__Password: "rig-rcon-password" },
      mem_limit: "5g",
      ports: ["9876:9876/udp", "9877:9877/udp"],
      volumes: ["server:/mnt/vrising/server"],
    },
  },
  enshrouded: {
    ready: /Server is up and listening/,
    timeout: 1_800_000,
    service: {
      image: "sknnr/enshrouded-dedicated-server:latest",
      environment: { SERVER_NAME: "Ludock real rig", SERVER_PASSWORD: "rig-password", SERVER_SLOTS: "4" },
      mem_limit: "5g",
      ports: ["15637:15637/udp"],
      // As the README mounts it: the image runs as uid 10000.
      volumes: ["savegame:/home/steam/enshrouded/savegame"],
    },
  },
  "enshrouded-mornedhels": {
    ready: /Server is up and listening|HostOnline/,
    timeout: 1_800_000,
    service: {
      image: "mornedhels/enshrouded-server:latest",
      environment: { SERVER_NAME: "Ludock real rig", SERVER_SLOT_COUNT: "4" },
      mem_limit: "5g",
      ports: ["15637:15637/udp"],
      stop_grace_period: "90s",
      // As the README mounts it: the server download, with the saves inside.
      volumes: ["game:/opt/enshrouded"],
    },
  },
  "7-days-to-die": {
    data: "/home/sdtdserver/.local/share/7DaysToDie",
    ready: /GameServer\.LogOn successful|StartGame done/,
    timeout: 2_700_000,
    service: {
      image: "vinanrra/7dtd-server:latest",
      environment: { START_MODE: "1", VERSION: "stable", PUID: "1000", PGID: "1000", TimeZone: "UTC" },
      mem_limit: "5g",
      ports: ["26900:26900/tcp", "26900-26902:26900-26902/udp"],
      // The README's other folders: LinuxGSM's config, the server, its logs and backups.
      volumes: ["config:/home/sdtdserver/lgsm/config-lgsm/sdtdserver", "server:/home/sdtdserver/serverfiles/",
        "log:/home/sdtdserver/log/", "backups:/home/sdtdserver/lgsm/backup/"],
      ulimits: { nofile: { soft: 10240, hard: 10240 } },
      stop_grace_period: "3m",
    },
  },
  "project-zomboid": {
    data: "/home/steam/Zomboid",
    ready: /SERVER STARTED/,
    timeout: 1_800_000,
    service: {
      image: "renegademaster/zomboid-dedicated-server:latest",
      environment: { ADMIN_PASSWORD: "rigadminpassword", RCON_PASSWORD: "rigrconpassword", SERVER_NAME: "LudockRig", MAX_RAM: "3072m" },
      mem_limit: "5g",
      ports: ["16261:16261/udp", "16262:16262/udp"],
      volumes: ["server:/home/steam/ZomboidDedicatedServer"],
    },
  },
};
