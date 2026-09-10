import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { Database } from "bun:sqlite";
import {
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
  keyedBindingFingerprint,
  keyedComposeSourceFingerprint,
} from "../src/database.js";
import {
  protectBindingFingerprint,
  protectComposeSourceFingerprint,
} from "../src/fingerprints.js";
import {
  applyMigrations as migrate,
  assertCompatibleDatabase,
  DATABASE_APPLICATION_ID,
  DATABASE_MIGRATIONS,
} from "../src/migrations.js";

const migrationKey = new Uint8Array(32).fill(42);
function applyMigrations(db: Database, migrations = DATABASE_MIGRATIONS): void {
  migrate(db, migrations, migrationKey);
}

const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ludock-migrations-"));
function schemaOneDatabase(name: string): string {
  const dbPath = path.join(directory, name);
  const db = new Database(dbPath);
  applyMigrations(db);
  db.exec("INSERT INTO docker_hosts VALUES ('host','local',1)");
  // Version 1 was a development-only marker and has no supported conversion.
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
  it("preserves foreign keys, busy timeout, and native result types", () => {
    const db = getDatabase();
    assert.equal(db.prepare("PRAGMA foreign_keys").get()?.foreign_keys, 1);
    assert.equal(db.prepare("PRAGMA busy_timeout").get()?.timeout, 5000);
    assert.equal(db.prepare("SELECT id FROM users WHERE id = 'missing'").get(), null);
    assert.equal(db.prepare("SELECT COUNT(*) AS count FROM users").get()?.count, 0);
    assert.throws(() => db.prepare(
      "INSERT INTO sessions (token_hash, session_id, user_id, created_at, expires_at, last_seen_at) VALUES ('token', 'session', 'missing', 1, 2, 1)",
    ).run(), /FOREIGN KEY/i);
    const statement = db.prepare("SELECT 1 AS value");
    closeDatabase();
    assert.throws(() => statement.get(), /closed/i);
  });

  it("initializes fresh storage and safely opens the same schema again", () => {
    const dbPath = path.join(directory, "fresh.db");
    process.env.LUDOCK_DB_PATH = dbPath;
    const db = getDatabase();
    const keyDirectory = `${fs.realpathSync(dbPath)}.identity-key`;
    const keyPath = path.join(keyDirectory, "key");
    assert.equal(
      db.prepare("PRAGMA application_id").get()?.application_id,
      DATABASE_APPLICATION_ID,
    );
    assert.equal(
      db.prepare("PRAGMA user_version").get()?.user_version,
      DATABASE_MIGRATIONS.at(-1)?.version,
    );
    assert.match(
      String(
        db.prepare("SELECT value_json FROM settings WHERE key='identity.key-check'")
          .get()?.value_json,
      ),
      /^"[a-f0-9]{64}"$/,
    );
    db.prepare(
      "INSERT INTO docker_hosts (id, name, created_at) VALUES ('host', 'test', 1)",
    ).run();
    assert.equal(fs.lstatSync(keyDirectory).isDirectory(), true);
    assert.equal(fs.statSync(keyDirectory).mode & 0o777, 0o700);
    const keyInfo = fs.lstatSync(keyPath);
    assert.equal(keyInfo.isFile(), true);
    assert.equal(keyInfo.nlink, 1);
    assert.equal(keyInfo.size, 32);
    assert.equal(keyInfo.mode & 0o777, 0o600);
    if (process.getuid) {
      assert.equal(fs.statSync(keyDirectory).uid, process.getuid());
      assert.equal(keyInfo.uid, process.getuid());
    }
    const key = fs.readFileSync(keyPath);
    assert.equal(
      fs.readdirSync(directory).some((entry) =>
        entry.startsWith("fresh.db.identity-key.stage-")),
      false,
    );
    closeDatabase();
    assert.equal(
      getDatabase().prepare("SELECT COUNT(*) AS count FROM docker_hosts").get()
        ?.count,
      1,
    );
    assert.deepEqual(fs.readFileSync(keyPath), key);
    assert.equal(fs.statSync(dbPath).mode & 0o777, 0o600);
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
    assert.throws(() => getDatabase(), /new application data volume/);
    assert.deepEqual(fs.readFileSync(dbPath), original);
    assert.equal(fs.statSync(dbPath).mode & 0o777, 0o640);
    assert.equal(fs.existsSync(`${dbPath}-wal`), false);
    assert.equal(fs.existsSync(`${dbPath}.identity-key`), false);
    // A failed open must not poison the process-wide database handle.
    process.env.LUDOCK_DB_PATH = ":memory:";
    assert.doesNotThrow(() => getDatabase());
  });

  it("does not infer database ownership from familiar table names", () => {
    const db = new Database(":memory:");
    try {
      db.exec("CREATE TABLE users (id TEXT)");
      assert.throws(() => applyMigrations(db), /incompatible with Ludock/);
      assert.equal(
        db.prepare("SELECT name FROM sqlite_schema WHERE type = 'table'").all()
          .length,
        1,
      );
    } finally {
      db.close(true);
    }
  });

  it("rejects the unsupported development schema without writing to it", () => {
    const dbPath = schemaOneDatabase("schema-one.db");
    fs.chmodSync(dbPath, 0o640);
    const original = fs.readFileSync(dbPath);
    process.env.LUDOCK_DB_PATH = dbPath;
    assert.throws(() => getDatabase(), /incompatible with Ludock/);
    assert.deepEqual(fs.readFileSync(dbPath), original);
    assert.equal(fs.statSync(dbPath).mode & 0o777, 0o640);
    assert.equal(fs.existsSync(`${dbPath}.identity-key`), false);
    assert.equal(fs.existsSync(`${dbPath}-wal`), false);
    assert.equal(fs.existsSync(`${dbPath}-journal`), false);
  });

  it("never publishes a partial key and recovers a durable key before schema commit", () => {
    const partialPath = path.join(directory, "partial-key.db");
    process.env.LUDOCK_DB_PATH = partialPath;
    spyOn(fs, "writeFileSync").mockImplementationOnce(() => {
      throw new Error("simulated interrupted key write");
    });
    assert.throws(() => getDatabase(), /identity key/);
    mock.restore();
    assert.equal(fs.existsSync(`${partialPath}.identity-key`), false);
    assert.equal(
      fs.readdirSync(directory).some((entry) =>
        entry.startsWith("partial-key.db.identity-key.stage-")),
      false,
    );

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
    assert.throws(() => getDatabase(), /identity key/);
    mock.restore();
    const keyPath = path.join(`${durablePath}.identity-key`, "key");
    assert.equal(fs.readFileSync(keyPath).length, 32);
    assert.equal(
      fs.readdirSync(directory).some((entry) =>
        entry.startsWith("durable-key.db.identity-key.stage-")),
      false,
    );
    const published = fs.readFileSync(keyPath);
    if (fs.statSync(durablePath).size > 0) {
      const uninitialized = new Database(durablePath, { readonly: true });
      assert.equal(
        uninitialized.prepare("PRAGMA user_version").get()?.user_version,
        0,
      );
      uninitialized.close(true);
    }
    const db = getDatabase();
    assert.equal(
      db.prepare("PRAGMA user_version").get()?.user_version,
      DATABASE_MIGRATIONS.at(-1)?.version,
    );
    assert.deepEqual(fs.readFileSync(keyPath), published);
  });

  it("rejects missing, replaced, and unsafe key directories without changing schema 2", () => {
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
      assert.throws(() => getDatabase(), /identity key/);
      assert.deepEqual(fs.readFileSync(dbPath), databaseBytes);
    };
    reject();
    assert.equal(fs.existsSync(keyDirectory), false);

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

    for (const key of [Buffer.alloc(3), Buffer.alloc(32, 1)]) {
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
    assert.doesNotThrow(() => getDatabase());
  });

  it("uses the canonical key directory through a database path alias", () => {
    const dbPath = path.join(directory, "canonical.db");
    process.env.LUDOCK_DB_PATH = dbPath;
    const fingerprint = keyedBindingFingerprint("a".repeat(64));
    closeDatabase();
    const alias = path.join(directory, "alias.db");
    fs.symlinkSync(dbPath, alias);
    process.env.LUDOCK_DB_PATH = alias;
    assert.equal(keyedBindingFingerprint("a".repeat(64)), fingerprint);
    assert.equal(fs.existsSync(`${alias}.identity-key`), false);
    assert.equal(fs.existsSync(path.join(`${dbPath}.identity-key`, "key")), true);
  });

  it("uses deterministic, separate HMAC domains for binding and Compose digests", () => {
    const digest = "a".repeat(64);
    const binding = protectBindingFingerprint(digest, migrationKey);
    const compose = protectComposeSourceFingerprint(digest, migrationKey);
    assert.match(binding, /^hmac-sha256:[a-f0-9]{64}$/);
    assert.match(compose, /^hmac-sha256:[a-f0-9]{64}$/);
    assert.notEqual(binding, compose);
    assert.equal(binding, protectBindingFingerprint(digest, migrationKey));
    assert.equal(compose, protectComposeSourceFingerprint(digest, migrationKey));
    assert.throws(() => protectBindingFingerprint("invalid", migrationKey));

    closeDatabase();
    process.env.LUDOCK_DB_PATH = ":memory:";
    assert.notEqual(
      keyedBindingFingerprint(digest),
      keyedComposeSourceFingerprint(digest),
    );
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
      assert.equal(
        db.prepare("SELECT username FROM users WHERE id = 'u'").get()?.username,
        "player",
      );
      assert.equal(
        db.prepare("PRAGMA user_version").get()?.user_version,
        nextVersion,
      );
      db.exec("INSERT INTO test_preferences VALUES ('u', 'preserved account')");
      assert.equal(db.prepare("SELECT note FROM test_preferences WHERE user_id = 'u'").get()?.note,
        "preserved account");
      assert.doesNotThrow(() => applyMigrations(db, migrations));
    } finally {
      db.close(true);
    }
  });

  it("rolls back an interrupted migration and leaves the prior version usable", () => {
    const db = new Database(":memory:");
    try {
      applyMigrations(db);
      const currentVersion = DATABASE_MIGRATIONS.at(-1)!.version;
      assert.throws(() =>
        applyMigrations(db, [
          ...DATABASE_MIGRATIONS,
          {
            version: currentVersion + 1,
            sql: "CREATE TABLE should_rollback (id TEXT); INSERT INTO nonexistent VALUES (1);",
          },
        ]),
      );
      assert.equal(db.prepare("PRAGMA user_version").get()?.user_version, currentVersion);
      assert.equal(
        db
          .prepare(
            "SELECT name FROM sqlite_schema WHERE name = 'should_rollback'",
          )
          .get(),
        null,
      );
      assert.doesNotThrow(() => applyMigrations(db));
    } finally {
      db.close(true);
    }
  });

  it("rejects newer schemas and unrelated application markers", () => {
    const db = new Database(":memory:");
    try {
      applyMigrations(db);
      db.exec("PRAGMA user_version = 999");
      assert.throws(() => assertCompatibleDatabase(db), /incompatible/);
      db.exec("PRAGMA user_version = 1; PRAGMA application_id = 123");
      assert.throws(() => applyMigrations(db), /incompatible/);
    } finally {
      db.close(true);
    }
  });
});
