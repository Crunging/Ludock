import { describe, expect, it } from "bun:test";
import { apiResponse } from "../src/api";
import {
  authUserResponseSchema,
  okResponseSchema,
} from "@ludock/shared";

describe("API response safety", () => {
  it("rejects malformed success bodies without exposing response contents", async () => {
    await expect(
      apiResponse(
        Response.json({ user: {
          id: "1675bade-833b-4c97-8bab-be48f44b5691",
          username: "admin",
          role: "secret-invalid-role",
        } }),
        authUserResponseSchema,
      ),
    ).rejects.toThrow("The server returned an invalid response.");
  });

  it("uses only a validated public error message with the original HTTP status", async () => {
    await expect(
      apiResponse(new Response("untrusted", { status: 502 }), okResponseSchema),
    ).rejects.toMatchObject({
      message: "Request failed (HTTP 502)",
      status: 502,
    });
  });
});
