import {
  closeDatabase,
  findUserByUsername,
  updateUserPassword,
  writeAuditLog,
} from "./database.js";
import { hashPassword } from "./auth.js";

async function main() {
  const username = process.argv[2]?.trim();
  const password = process.env.LUDOCK_RECOVERY_PASSWORD;

  if (!username || !password) {
    console.error(
      "Usage: LUDOCK_RECOVERY_PASSWORD='<new password>' node dist/recovery.js <username>"
    );
    process.exitCode = 2;
    return;
  }
  if (password.length < 15 || password.length > 128) {
    console.error("Recovery password must be between 15 and 128 characters.");
    process.exitCode = 2;
    return;
  }

  const user = findUserByUsername(username);
  if (!user) {
    console.error(`Account "${username}" was not found.`);
    process.exitCode = 1;
    return;
  }

  updateUserPassword(user.id, await hashPassword(password));
  writeAuditLog({
    action: "user.password.recovered",
    targetType: "user",
    targetId: user.id,
    details: { username: user.username },
  });
  console.log(
    `Password reset for "${user.username}". All existing sessions were revoked.`
  );
}

main()
  .catch((error: unknown) => {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  })
  .finally(closeDatabase);
