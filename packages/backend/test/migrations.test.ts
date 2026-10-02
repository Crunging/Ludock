import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { Database } from "bun:sqlite";
import { expect,
  afterAll as after,
  afterEach,
  beforeEach,
  describe,
  it,
  mock,
  spyOn,
} from "bun:test";
import {
  closeDatabase,
  getDatabase,
} from "../src/database.js";
import {
  applyMigrations as migrate,
  assertCompatibleDatabase,
  DATABASE_APPLICATION_ID,
  DATABASE_MIGRATIONS,
} from "../src/migrations.js";
import type { SQLQueryBindings } from "bun:sqlite";

const migrationKey = new Uint8Array(32).fill(42);
function applyMigrations(db: Database, migrations = DATABASE_MIGRATIONS): void {
  migrate(db, migrations, migrationKey);
}

const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ludock-migrations-"));
function olderSchemaDatabase(name: string): string {
  const dbPath = path.join(directory, name);
  const db = new Database(dbPath);
  applyMigrations(db);
  db.exec("INSERT INTO docker_hosts VALUES ('host','local',1)");
  // Development schemas before the first release have no supported conversion.
  db.exec("PRAGMA user_version = 1");
  db.close(true);
  return dbPath;
}
beforeEach(() => {
  closeDatabase();
  process.env.LUDOCK_DB_PATH = ":memory:";
});
afterEach(() => mock.restore());
after(() => {
  closeDatabase();
  fs.rmSync(directory, { recursive: true, force: true });
});

describe("application database ownership and schema migrations", () => {
  it("initializes fresh storage and safely opens the same schema again", () => {
    const dbPath = path.join(directory, "fresh.db");
    process.env.LUDOCK_DB_PATH = dbPath;
    const db = getDatabase();
    const keyDirectory = `${fs.realpathSync(dbPath)}.identity-key`;
    const keyPath = path.join(keyDirectory, "key");
    expect(db.prepare<Record<string, unknown>, SQLQueryBindings[]>("PRAGMA application_id").get()?.application_id).toBe(DATABASE_APPLICATION_ID);
    expect(db.prepare<Record<string, unknown>, SQLQueryBindings[]>("PRAGMA user_version").get()?.user_version).toBe(DATABASE_MIGRATIONS.at(-1)?.version);
    expect(String(
        db.prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT value_json FROM settings WHERE key='identity.key-check'")
          .get()?.value_json,
      )).toMatch(/^"[a-f0-9]{64}"$/);
    db.prepare(
      "INSERT INTO docker_hosts (id, name, created_at) VALUES ('host', 'test', 1)",
    ).run();
    expect(fs.lstatSync(keyDirectory).isDirectory()).toBe(true);
    expect(fs.statSync(keyDirectory).mode & 0o777).toBe(0o700);
    const keyInfo = fs.lstatSync(keyPath);
    expect(keyInfo.isFile()).toBe(true);
    expect(keyInfo.nlink).toBe(1);
    expect(keyInfo.size).toBe(32);
    expect(keyInfo.mode & 0o777).toBe(0o600);
    if (process.getuid) {
      expect(fs.statSync(keyDirectory).uid).toBe(process.getuid());
      expect(keyInfo.uid).toBe(process.getuid());
    }
    const key = fs.readFileSync(keyPath);
    expect(fs.readdirSync(directory).some((entry) =>
        entry.startsWith("fresh.db.identity-key.stage-"))).toBe(false);
    closeDatabase();
    expect(getDatabase().prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT COUNT(*) AS count FROM docker_hosts").get()
        ?.count).toBe(1);
    expect(fs.readFileSync(keyPath)).toStrictEqual(key);
    expect(fs.statSync(dbPath).mode & 0o777).toBe(0o600);
  });

  it("rejects an unrelated database without changing its bytes, schema, or permissions", () => {
    const dbPath = path.join(directory, "unrelated.db");
    const unrelated = new Database(dbPath);
    unrelated.exec(
      "CREATE TABLE inventory_items (id TEXT, description TEXT); INSERT INTO inventory_items VALUES ('asset-1', 'preserve-this');",
    );
    unrelated.close(true);
    fs.chmodSync(dbPath, 0o640);
    const original = fs.readFileSync(dbPath);
    process.env.LUDOCK_DB_PATH = dbPath;
    expect(() => getDatabase()).toThrow(/new application data volume/);
    expect(fs.readFileSync(dbPath)).toStrictEqual(original);
    expect(fs.statSync(dbPath).mode & 0o777).toBe(0o640);
    expect(fs.existsSync(`${dbPath}-wal`)).toBe(false);
    expect(fs.existsSync(`${dbPath}.identity-key`)).toBe(false);
    // A failed open must not poison the process-wide database handle.
    process.env.LUDOCK_DB_PATH = ":memory:";
    expect(() => getDatabase()).not.toThrow();
  });

  it("rejects an unsupported older schema without writing to it", () => {
    const dbPath = olderSchemaDatabase("older-schema.db");
    fs.chmodSync(dbPath, 0o640);
    const original = fs.readFileSync(dbPath);
    process.env.LUDOCK_DB_PATH = dbPath;
    expect(() => getDatabase()).toThrow(/incompatible with Ludock/);
    expect(fs.readFileSync(dbPath)).toStrictEqual(original);
    expect(fs.statSync(dbPath).mode & 0o777).toBe(0o640);
    expect(fs.existsSync(`${dbPath}.identity-key`)).toBe(false);
    expect(fs.existsSync(`${dbPath}-wal`)).toBe(false);
    expect(fs.existsSync(`${dbPath}-journal`)).toBe(false);
  });

  it("never publishes a partial key and recovers a durable key before schema commit", () => {
    const partialPath = path.join(directory, "partial-key.db");
    process.env.LUDOCK_DB_PATH = partialPath;
    spyOn(fs, "writeFileSync").mockImplementationOnce(() => {
      throw new Error("simulated interrupted key write");
    });
    expect(() => getDatabase()).toThrow(/identity key/);
    mock.restore();
    expect(fs.existsSync(`${partialPath}.identity-key`)).toBe(false);
    expect(fs.readdirSync(directory).some((entry) =>
        entry.startsWith("partial-key.db.identity-key.stage-"))).toBe(false);

    const durablePath = path.join(directory, "durable-key.db");
    process.env.LUDOCK_DB_PATH = durablePath;
    const originalFsync = fs.fsyncSync.bind(fs);
    let syncCount = 0;
    spyOn(fs, "fsyncSync").mockImplementation((descriptor) => {
      syncCount++;
      if (syncCount === 3)
        throw new Error("simulated interruption before parent sync");
      return originalFsync(descriptor);
    });
    expect(() => getDatabase()).toThrow(/identity key/);
    mock.restore();
    const keyPath = path.join(`${durablePath}.identity-key`, "key");
    expect(fs.readFileSync(keyPath).length).toBe(32);
    expect(fs.readdirSync(directory).some((entry) =>
        entry.startsWith("durable-key.db.identity-key.stage-"))).toBe(false);
    const published = fs.readFileSync(keyPath);
    if (fs.statSync(durablePath).size > 0) {
      const uninitialized = new Database(durablePath, { readonly: true });
      expect(uninitialized.prepare<Record<string, unknown>, SQLQueryBindings[]>("PRAGMA user_version").get()?.user_version).toBe(0);
      uninitialized.close(true);
    }
    const db = getDatabase();
    expect(db.prepare<Record<string, unknown>, SQLQueryBindings[]>("PRAGMA user_version").get()?.user_version).toBe(DATABASE_MIGRATIONS.at(-1)?.version);
    expect(fs.readFileSync(keyPath)).toStrictEqual(published);
  });

  it("rejects missing, replaced, and unsafe key directories without changing the database", () => {
    const dbPath = path.join(directory, "key-validation.db");
    process.env.LUDOCK_DB_PATH = dbPath;
    getDatabase();
    closeDatabase();
    const keyDirectory = `${dbPath}.identity-key`;
    const savedDirectory = `${keyDirectory}.saved`;
    const savedKeyPath = path.join(savedDirectory, "key");
    const databaseBytes = fs.readFileSync(dbPath);
    fs.renameSync(keyDirectory, savedDirectory);

    const reject = () => {
      expect(() => getDatabase()).toThrow(/identity key/);
      expect(fs.readFileSync(dbPath)).toStrictEqual(databaseBytes);
    };
    reject();
    expect(fs.existsSync(keyDirectory)).toBe(false);

    fs.writeFileSync(keyDirectory, "not-a-directory", { mode: 0o600 });
    reject();
    fs.unlinkSync(keyDirectory);

    fs.mkdirSync(keyDirectory, { mode: 0o700 });
    reject();
    fs.rmdirSync(keyDirectory);

    fs.symlinkSync(savedDirectory, keyDirectory, "dir");
    reject();
    fs.unlinkSync(keyDirectory);

    fs.mkdirSync(keyDirectory, { mode: 0o755 });
    fs.copyFileSync(savedKeyPath, path.join(keyDirectory, "key"));
    fs.chmodSync(path.join(keyDirectory, "key"), 0o600);
    reject();
    fs.rmSync(keyDirectory, { recursive: true });

    for (const key of [new Uint8Array(3), new Uint8Array(32).fill(1)]) {
      fs.mkdirSync(keyDirectory, { mode: 0o700 });
      fs.writeFileSync(path.join(keyDirectory, "key"), key, { mode: 0o600 });
      reject();
      fs.rmSync(keyDirectory, { recursive: true });
    }

    fs.mkdirSync(keyDirectory, { mode: 0o700 });
    fs.copyFileSync(savedKeyPath, path.join(keyDirectory, "key"));
    fs.chmodSync(path.join(keyDirectory, "key"), 0o644);
    reject();
    fs.rmSync(keyDirectory, { recursive: true });

    fs.mkdirSync(keyDirectory, { mode: 0o700 });
    fs.symlinkSync(savedKeyPath, path.join(keyDirectory, "key"));
    reject();
    fs.rmSync(keyDirectory, { recursive: true });

    fs.mkdirSync(keyDirectory, { mode: 0o700 });
    fs.linkSync(savedKeyPath, path.join(keyDirectory, "key"));
    reject();
    fs.rmSync(keyDirectory, { recursive: true });

    fs.renameSync(savedDirectory, keyDirectory);
    expect(() => getDatabase()).not.toThrow();
  });

  it("applies a future schema addition without replacing existing records", () => {
    const db = new Database(":memory:");
    try {
      applyMigrations(db);
      db.exec("INSERT INTO users VALUES ('u','player','hash','viewer',0,1,1)");
      const nextVersion = DATABASE_MIGRATIONS.at(-1)!.version + 1;
      const migrations = [
        ...DATABASE_MIGRATIONS,
        {
          version: nextVersion,
          sql: "CREATE TABLE test_preferences (user_id TEXT PRIMARY KEY REFERENCES users(id), note TEXT NOT NULL) STRICT;",
        },
      ];
      applyMigrations(db, migrations);
      expect(db.prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT username FROM users WHERE id = 'u'").get()?.username).toBe("player");
      expect(db.prepare<Record<string, unknown>, SQLQueryBindings[]>("PRAGMA user_version").get()?.user_version).toBe(nextVersion);
      db.exec("INSERT INTO test_preferences VALUES ('u', 'preserved account')");
      expect(db.prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT note FROM test_preferences WHERE user_id = 'u'").get()?.note).toBe("preserved account");
      expect(() => applyMigrations(db, migrations)).not.toThrow();
    } finally {
      db.close(true);
    }
  });

  it("rolls back an interrupted migration and leaves the prior version usable", () => {
    const db = new Database(":memory:");
    try {
      applyMigrations(db);
      const currentVersion = DATABASE_MIGRATIONS.at(-1)!.version;
      expect(() =>
        applyMigrations(db, [
          ...DATABASE_MIGRATIONS,
          {
            version: currentVersion + 1,
            sql: "CREATE TABLE should_rollback (id TEXT); INSERT INTO nonexistent VALUES (1);",
          },
        ])).toThrow();
      expect(db.prepare<Record<string, unknown>, SQLQueryBindings[]>("PRAGMA user_version").get()?.user_version).toBe(currentVersion);
      expect(db
          .prepare<Record<string, unknown>, SQLQueryBindings[]>(
            "SELECT name FROM sqlite_schema WHERE name = 'should_rollback'",
          )
          .get()).toBe(null);
      expect(() => applyMigrations(db)).not.toThrow();
    } finally {
      db.close(true);
    }
  });

  it("rejects newer schemas and unrelated application markers", () => {
    const db = new Database(":memory:");
    try {
      applyMigrations(db);
      db.exec("PRAGMA user_version = 999");
      expect(() => assertCompatibleDatabase(db)).toThrow(/incompatible/);
      db.exec("PRAGMA user_version = 1; PRAGMA application_id = 123");
      expect(() => applyMigrations(db)).toThrow(/incompatible/);
    } finally {
      db.close(true);
    }
  });
});
