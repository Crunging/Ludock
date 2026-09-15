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

function legacyScheduleDatabase(): Database {
  const db = new Database(":memory:");
  applyMigrations(db, DATABASE_MIGRATIONS.filter(({ version }) => version <= 3));
  db.exec("INSERT INTO users VALUES ('owner','owner','hash','operator',0,1,1), ('other','other','hash','operator',0,1,1)");
  db.exec("INSERT INTO docker_hosts VALUES ('host','local',1)");
  db.exec(`INSERT INTO logical_servers (
    id,host_id,external_identity,container_id,display_name,game_type,status,
    binding_revision,binding_fingerprint,first_seen_at,last_seen_at
  ) VALUES
    ('server','host','world','container','World','minecraft','active',1,'fingerprint',1,1),
    ('other','host','other-world','other-container','Other world','minecraft','active',1,'other-fingerprint',1,1)`);
  return db;
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
    expect(db.prepare("PRAGMA foreign_keys").get()?.foreign_keys).toBe(1);
    expect(db.prepare("PRAGMA busy_timeout").get()?.timeout).toBe(5000);
    expect(db.prepare("SELECT id FROM users WHERE id = 'missing'").get()).toBe(null);
    expect(db.prepare("SELECT COUNT(*) AS count FROM users").get()?.count).toBe(0);
    expect(() => db.prepare(
      "INSERT INTO sessions (token_hash, session_id, user_id, created_at, expires_at, last_seen_at) VALUES ('token', 'session', 'missing', 1, 2, 1)",
    ).run()).toThrow(/FOREIGN KEY/i);
    const statement = db.prepare("SELECT 1 AS value");
    closeDatabase();
    expect(() => statement.get()).toThrow(/closed/i);
  });

  it("initializes fresh storage and safely opens the same schema again", () => {
    const dbPath = path.join(directory, "fresh.db");
    process.env.LUDOCK_DB_PATH = dbPath;
    const db = getDatabase();
    const keyDirectory = `${fs.realpathSync(dbPath)}.identity-key`;
    const keyPath = path.join(keyDirectory, "key");
    expect(db.prepare("PRAGMA application_id").get()?.application_id).toBe(DATABASE_APPLICATION_ID);
    expect(db.prepare("PRAGMA user_version").get()?.user_version).toBe(DATABASE_MIGRATIONS.at(-1)?.version);
    expect(String(
        db.prepare("SELECT value_json FROM settings WHERE key='identity.key-check'")
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
    expect(getDatabase().prepare("SELECT COUNT(*) AS count FROM docker_hosts").get()
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

  it("does not infer database ownership from familiar table names", () => {
    const db = new Database(":memory:");
    try {
      db.exec("CREATE TABLE users (id TEXT)");
      expect(() => applyMigrations(db)).toThrow(/incompatible with Ludock/);
      expect(db.prepare("SELECT name FROM sqlite_schema WHERE type = 'table'").all()
          .length).toBe(1);
    } finally {
      db.close(true);
    }
  });

  it("rejects the unsupported development schema without writing to it", () => {
    const dbPath = schemaOneDatabase("schema-one.db");
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
      expect(uninitialized.prepare("PRAGMA user_version").get()?.user_version).toBe(0);
      uninitialized.close(true);
    }
    const db = getDatabase();
    expect(db.prepare("PRAGMA user_version").get()?.user_version).toBe(DATABASE_MIGRATIONS.at(-1)?.version);
    expect(fs.readFileSync(keyPath)).toStrictEqual(published);
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

  it("uses the canonical key directory through a database path alias", () => {
    const dbPath = path.join(directory, "canonical.db");
    process.env.LUDOCK_DB_PATH = dbPath;
    const fingerprint = keyedBindingFingerprint("a".repeat(64));
    closeDatabase();
    const alias = path.join(directory, "alias.db");
    fs.symlinkSync(dbPath, alias);
    process.env.LUDOCK_DB_PATH = alias;
    expect(keyedBindingFingerprint("a".repeat(64))).toBe(fingerprint);
    expect(fs.existsSync(`${alias}.identity-key`)).toBe(false);
    expect(fs.existsSync(path.join(`${dbPath}.identity-key`, "key"))).toBe(true);
  });

  it("uses deterministic, separate HMAC domains for binding and Compose digests", () => {
    const digest = "a".repeat(64);
    const binding = protectBindingFingerprint(digest, migrationKey);
    const compose = protectComposeSourceFingerprint(digest, migrationKey);
    expect(binding).toMatch(/^hmac-sha256:[a-f0-9]{64}$/);
    expect(compose).toMatch(/^hmac-sha256:[a-f0-9]{64}$/);
    expect(binding).not.toBe(compose);
    expect(binding).toBe(protectBindingFingerprint(digest, migrationKey));
    expect(compose).toBe(protectComposeSourceFingerprint(digest, migrationKey));
    expect(() => protectBindingFingerprint("invalid", migrationKey)).toThrow();

    closeDatabase();
    process.env.LUDOCK_DB_PATH = ":memory:";
    expect(keyedBindingFingerprint(digest)).not.toBe(keyedComposeSourceFingerprint(digest));
  });

  it("adds schedule revisions while preserving existing schedules and queued work", () => {
    const db = new Database(":memory:");
    try {
      applyMigrations(db, DATABASE_MIGRATIONS.slice(0, 1));
      const settings = JSON.stringify({ action: "start", enabled: true, time: "08:00", days: [1], timezone: "UTC" });
      db.exec("INSERT INTO users VALUES ('u','player','hash','operator',0,1,1)");
      db.exec("INSERT INTO docker_hosts VALUES ('host','local',1)");
      db.exec(`INSERT INTO logical_servers (
        id,host_id,external_identity,container_id,display_name,game_type,status,
        binding_revision,binding_fingerprint,first_seen_at,last_seen_at
      ) VALUES ('server','host','world','container','World','minecraft','active',1,'fingerprint',1,1)`);
      db.prepare(`INSERT INTO schedules (
        id,server_id,owner_id,input_json,binding_revision,last_slot,last_result,created_at
      ) VALUES ('schedule','server','u',?,1,'consumed slot','previous result',1)`)
        .run(settings);
      db.exec(`INSERT INTO operations (
        id,server_id,actor_id,kind,status,phase,input_json,binding_revision,created_at,updated_at
      ) VALUES ('operation','server','u','start','queued','queued','{"scheduleId":"schedule"}',1,1,1)`);
      applyMigrations(db);
      const schedule = db.prepare("SELECT * FROM schedules WHERE id='schedule'").get();
      expect(schedule?.revision).toBe(1);
      expect(schedule?.input_json).toBe(settings);
      expect(schedule?.owner_id).toBe("u");
      expect(schedule?.binding_revision).toBe(1);
      expect(schedule?.last_slot).toBe("consumed slot");
      expect(schedule?.last_result).toBe("previous result");
      expect(db.prepare("SELECT input_json FROM operations WHERE id='operation'").get()?.input_json).toBe('{"scheduleId":"schedule"}');
      expect(() => db.exec("UPDATE schedules SET revision=0")).toThrow(/CHECK/);
    } finally {
      db.close(true);
    }
  });

  it("associates matching legacy schedule operations without rewriting queued or finished history", () => {
    const db = legacyScheduleDatabase();
    try {
      const insertSchedule = db.prepare(`INSERT INTO schedules (
        id,server_id,owner_id,input_json,binding_revision,last_slot,last_result,created_at,revision
      ) VALUES (?,'server','owner','{}',1,'consumed slot',?,1,3)`);
      const insertOperation = db.prepare(`INSERT INTO operations (
        id,server_id,actor_id,kind,status,phase,input_json,binding_revision,created_at,updated_at,error,result_json
      ) VALUES (?,'server','owner','start',?,'recorded phase',?,1,25,50,?,?)`);
      for (const status of ["queued", "succeeded", "failed", "cancelled"]) {
        insertSchedule.run(status, `Queued operation operation-${status}`);
        insertOperation.run(`operation-${status}`, status, JSON.stringify({ scheduleId: status }),
          status === "failed" ? "Previous failure" : null,
          status === "succeeded" ? '{"message":"Completed"}' : null);
      }
      const originalSchedules = db.prepare("SELECT * FROM schedules ORDER BY id").all();
      const originalOperations = db.prepare("SELECT * FROM operations ORDER BY id").all();

      applyMigrations(db);

      expect(db.prepare("SELECT * FROM schedules ORDER BY id").all()).toStrictEqual(originalSchedules.map((schedule) => ({
          ...schedule,
          last_operation_id: `operation-${schedule.id}`,
          last_run_at: 25,
        })));
      expect(db.prepare("SELECT * FROM operations ORDER BY id").all()).toStrictEqual(originalOperations);
      const migrated = db.prepare("SELECT * FROM schedules ORDER BY id").all();
      applyMigrations(db);
      expect(db.prepare("SELECT * FROM schedules ORDER BY id").all()).toStrictEqual(migrated);
      expect(db.prepare("SELECT * FROM operations ORDER BY id").all()).toStrictEqual(originalOperations);
      expect(() => db.exec("UPDATE schedules SET last_run_at = -1")).toThrow(/CHECK/);
      expect(() => db.exec("UPDATE schedules SET last_run_at = NULL")).not.toThrow();
    } finally {
      db.close(true);
    }
  });

  it("preserves unverified legacy schedule history without associating unrelated or malformed operations", () => {
    const db = legacyScheduleDatabase();
    try {
      const cases: {
        id: string;
        serverId?: string;
        ownerId?: string;
        input?: string;
        result?: string | null;
        createdAt?: number;
        missing?: boolean;
      }[] = [
        { id: "wrong-server", serverId: "other" },
        { id: "wrong-owner", ownerId: "other" },
        { id: "wrong-schedule", input: '{"scheduleId":"another-schedule"}' },
        { id: "missing-schedule", input: "{}" },
        { id: "malformed", input: '{"scheduleId":' },
        { id: "array", input: '[{"scheduleId":"array"}]' },
        { id: "null", input: "null" },
        { id: "string", input: '"string"' },
        { id: "number", input: "1" },
        { id: "boolean", input: "true" },
        { id: "nonstring-schedule", input: '{"scheduleId":["nonstring-schedule"]}' },
        { id: "prefix", result: "Previously Queued operation operation-prefix" },
        { id: "suffix", result: "Queued operation operation-suffix completed" },
        { id: "newline", result: "Queued operation operation-newline\n" },
        { id: "wrong-case", result: "queued operation operation-wrong-case" },
        { id: "missing-operation", missing: true },
        { id: "negative-time", createdAt: -1 },
        { id: "error-history", result: "Permission denied" },
        { id: "never-run", result: null },
      ];
      const insertSchedule = db.prepare(`INSERT INTO schedules (
        id,server_id,owner_id,input_json,binding_revision,last_slot,last_result,created_at
      ) VALUES (?,'server','owner','{}',1,'previous slot',?,1)`);
      const insertOperation = db.prepare(`INSERT INTO operations (
        id,server_id,actor_id,kind,status,phase,input_json,binding_revision,created_at,updated_at
      ) VALUES (?,?,?,'start','queued','queued',?,1,?,50)`);
      for (const entry of cases) {
        insertSchedule.run(entry.id,
          entry.result === undefined ? `Queued operation operation-${entry.id}` : entry.result);
        if (!entry.missing) {
          insertOperation.run(`operation-${entry.id}`, entry.serverId ?? "server", entry.ownerId ?? "owner",
            entry.input ?? JSON.stringify({ scheduleId: entry.id }), entry.createdAt ?? 25);
        }
      }
      const originalSchedules = db.prepare("SELECT * FROM schedules ORDER BY id").all();
      const originalOperations = db.prepare("SELECT * FROM operations ORDER BY id").all();

      expect(() => applyMigrations(db)).not.toThrow();

      expect(db.prepare("SELECT * FROM schedules ORDER BY id").all()).toStrictEqual(originalSchedules.map((schedule) => ({ ...schedule, last_operation_id: null, last_run_at: null })));
      expect(db.prepare("SELECT * FROM operations ORDER BY id").all()).toStrictEqual(originalOperations);
    } finally {
      db.close(true);
    }
  });

  it("upgrades notification history without changing deliveries, counters, or event deduplication", () => {
    const db = new Database(":memory:");
    try {
      applyMigrations(db, DATABASE_MIGRATIONS.filter(({ version }) => version <= 4));
      const insert = db.prepare(`INSERT INTO notification_deliveries
        (id,event_key,payload_json,attempts,next_attempt_at,state,created_at)
        VALUES (?,?,?,?,?,?,?)`);
      insert.run("queued", "event-queued", '{"content":"queued message"}', 2, 900, "queued", 100);
      insert.run("delivered", "event-delivered", '{"content":"delivered message"}', 1, 800, "delivered", 200);
      insert.run("failed", "event-failed", '{"content":"failed message"}', 5, 700, "failed", 300);
      const original = db.prepare("SELECT * FROM notification_deliveries ORDER BY id").all();

      applyMigrations(db);

      const migrated = db.prepare("SELECT * FROM notification_deliveries ORDER BY id").all();
      expect(migrated).toStrictEqual(original.map((delivery) => ({
        ...delivery,
        kind: "event",
        retry_attempts: delivery.attempts,
        last_attempt_at: null,
        delivered_at: null,
        failure_code: null,
      })));
      applyMigrations(db);
      expect(db.prepare("SELECT * FROM notification_deliveries ORDER BY id").all()).toStrictEqual(migrated);
      expect(() => insert.run("duplicate", "event-queued", "{}", 0, 1000, "queued", 400)).toThrow(/UNIQUE/);
      expect(db.prepare("SELECT COUNT(*) AS count FROM notification_deliveries").get()?.count).toBe(3);
    } finally {
      db.close(true);
    }
  });

  it("indexes existing history without rewriting events or rejecting legacy malformed details", () => {
    const db = legacyScheduleDatabase();
    try {
      applyMigrations(db, DATABASE_MIGRATIONS.filter(({ version }) => version <= 5));
      db.exec(`INSERT INTO notification_deliveries
        (id,event_key,payload_json,attempts,next_attempt_at,state,created_at,kind,retry_attempts,last_attempt_at,failure_code)
        VALUES ('delivery','event','{}',7,30,'failed',10,'test',5,20,'network_error')`);
      db.exec(`INSERT INTO operations (
        id,server_id,actor_id,kind,status,phase,input_json,recovery_json,binding_revision,created_at,updated_at
      ) VALUES ('operation','server','owner','backup','running','copying','{"scheduleId":"schedule"}','{"initiallyRunning":true}',1,10,20)`);
      const insert = db.prepare("INSERT INTO audit_log (user_id,action,target_type,target_id,details_json,created_at) VALUES ('owner','server.backup.queued','server','server',?,10)");
      for (const details of ['{"operationId":"operation"}', '{"operationId":', 'null', '["operation"]', '{"operationId":42}'])
        insert.run(details);
      const events = db.prepare("SELECT * FROM audit_log ORDER BY id").all();
      const operations = db.prepare("SELECT * FROM operations").all();
      const deliveries = db.prepare("SELECT * FROM notification_deliveries").all();

      expect(() => applyMigrations(db)).not.toThrow();

      expect(db.prepare("SELECT * FROM audit_log ORDER BY id").all()).toStrictEqual(events);
      expect(db.prepare("SELECT * FROM operations").all()).toStrictEqual(operations);
      expect(db.prepare("SELECT * FROM notification_deliveries").all()).toStrictEqual(deliveries);
      expect(() => insert.run('{"operationId":')).not.toThrow();
    } finally {
      db.close(true);
    }
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
      expect(db.prepare("SELECT username FROM users WHERE id = 'u'").get()?.username).toBe("player");
      expect(db.prepare("PRAGMA user_version").get()?.user_version).toBe(nextVersion);
      db.exec("INSERT INTO test_preferences VALUES ('u', 'preserved account')");
      expect(db.prepare("SELECT note FROM test_preferences WHERE user_id = 'u'").get()?.note).toBe("preserved account");
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
      expect(db.prepare("PRAGMA user_version").get()?.user_version).toBe(currentVersion);
      expect(db
          .prepare(
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
