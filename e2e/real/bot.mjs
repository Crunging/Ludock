// A headless Minecraft Java player for the real rig: it joins, stays, chats
// once a minute, and prints what a player sees, one timestamped line each.
import { createRequire } from "node:module";

const require = createRequire(process.env.NODE_PATH + "/");
const mineflayer = require("mineflayer");
const [name, version] = process.argv.slice(2);
const log = (...parts) => console.log(new Date().toISOString(), name, ...parts);

const bot = mineflayer.createBot({ host: "127.0.0.1", port: 25565, username: name, version, auth: "offline" });
let heard = Date.now();
let silent = false;

bot.once("spawn", () => {
  log("joined at", bot.entity.position.floored().toString());
  bot.chat(`hello from ${name}`);
  setInterval(() => bot.chat(`${name} is still here`), 60_000);
  setInterval(() => {
    const quiet = Math.round((Date.now() - heard) / 1000);
    if (quiet >= 10 && !silent) log(`server silent for ${quiet} s`);
    silent = quiet >= 10;
  }, 1000);
});
// The server sends the time every second while its game loop runs.
bot.on("time", () => {
  if (silent) log("server answering again");
  heard = Date.now();
  silent = false;
});
bot.on("messagestr", (message, position) => log(`[${position}]`, message));
bot.on("playerJoined", (player) => player.username !== name && log("player joined:", player.username));
bot.on("playerLeft", (player) => player.username !== name && log("player left:", player.username));
bot.on("kicked", (reason) => log("kicked:", typeof reason === "string" ? reason : JSON.stringify(reason)));
bot.on("error", (error) => log("error:", error.message));
bot.on("end", (reason) => {
  log("disconnected:", reason);
  process.exit(0);
});
process.on("SIGTERM", () => bot.quit());
process.on("SIGINT", () => bot.quit());
