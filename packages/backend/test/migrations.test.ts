import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { DatabaseSync } from "node:sqlite";
import { after, beforeEach, describe, it } from "node:test";
import { closeDatabase, getDatabase } from "../src/database.js";
import {
  applyMigrations,
  assertCompatibleDatabase,
  DATABASE_APPLICATION_ID,
  DATABASE_MIGRATIONS,
} from "../src/migrations.js";

const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ludock-migrations-"));
beforeEach(() => {
  closeDatabase();
  process.env.LUDOCK_DB_PATH = ":memory:";
});
after(() => {
  closeDatabase();
  fs.rmSync(directory, { recursive: true, force: true });
});

describe("application database ownership and schema migrations", () => {
  it("initializes fresh storage and safely opens the same schema again", () => {
    const dbPath = path.join(directory, "fresh.db");
    process.env.LUDOCK_DB_PATH = dbPath;
    const db = getDatabase();
    assert.equal(
      db.prepare("PRAGMA application_id").get()?.application_id,
      DATABASE_APPLICATION_ID,
    );
    assert.equal(
      db.prepare("PRAGMA user_version").get()?.user_version,
      DATABASE_MIGRATIONS.at(-1)?.version,
    );
    db.prepare(
      "INSERT INTO docker_hosts (id, name, created_at) VALUES ('host', 'test', 1)",
    ).run();
    closeDatabase();
    assert.equal(
      getDatabase().prepare("SELECT COUNT(*) AS count FROM docker_hosts").get()
        ?.count,
      1,
    );
    assert.equal(fs.statSync(dbPath).mode & 0o777, 0o600);
  });

  it("rejects an unrelated database without changing its bytes, schema, or permissions", () => {
    const dbPath = path.join(directory, "unrelated.db");
    const unrelated = new DatabaseSync(dbPath);
    unrelated.exec(
      "CREATE TABLE inventory_items (id TEXT, description TEXT); INSERT INTO inventory_items VALUES ('asset-1', 'preserve-this');",
    );
    unrelated.close();
    fs.chmodSync(dbPath, 0o640);
    const original = fs.readFileSync(dbPath);
    process.env.LUDOCK_DB_PATH = dbPath;
    assert.throws(() => getDatabase(), /new application data volume/);
    assert.deepEqual(fs.readFileSync(dbPath), original);
    assert.equal(fs.statSync(dbPath).mode & 0o777, 0o640);
    assert.equal(fs.existsSync(`${dbPath}-wal`), false);
    // A failed open must not poison the process-wide database handle.
    process.env.LUDOCK_DB_PATH = ":memory:";
    assert.doesNotThrow(() => getDatabase());
  });

  it("does not infer database ownership from familiar table names", () => {
    const db = new DatabaseSync(":memory:");
    try {
      db.exec("CREATE TABLE users (id TEXT)");
      assert.throws(() => applyMigrations(db), /incompatible with Ludock/);
      assert.equal(
        db.prepare("SELECT name FROM sqlite_schema WHERE type = 'table'").all()
          .length,
        1,
      );
    } finally {
      db.close();
    }
  });

  it("applies a future schema addition without replacing existing records", () => {
    const db = new DatabaseSync(":memory:");
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
      db.close();
    }
  });

  it("rolls back an interrupted migration and leaves the prior version usable", () => {
    const db = new DatabaseSync(":memory:");
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
        undefined,
      );
      assert.doesNotThrow(() => applyMigrations(db));
    } finally {
      db.close();
    }
  });

  it("rejects newer schemas and unrelated application markers", () => {
    const db = new DatabaseSync(":memory:");
    try {
      applyMigrations(db);
      db.exec("PRAGMA user_version = 999");
      assert.throws(() => assertCompatibleDatabase(db), /incompatible/);
      db.exec("PRAGMA user_version = 1; PRAGMA application_id = 123");
      assert.throws(() => applyMigrations(db), /incompatible/);
    } finally {
      db.close();
    }
  });
});
