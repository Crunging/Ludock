import { encodeText } from "./bytes.js";
import type { ServerWebSocket } from "bun";
import { createLogger } from "./logger.js";

export type SocketMessage = string | Uint8Array | ArrayBuffer;
export const MAX_SOCKET_BUFFER_BYTES = 1024 * 1024;
const logger = createLogger("websocket");

/** The console, logs, and event stream need only these connection operations. */
export interface SocketChannel {
  readonly isOpen: boolean;
  send(message: string): void;
  close(code: number, reason?: string): void;
  onMessage(listener: (message: SocketMessage) => void): void;
  onClose(listener: (code: number) => void): void;
}

export class NativeSocketChannel implements SocketChannel {
  private closed = false;
  private closeCode = 1000;
  private readonly messages = new Set<(message: SocketMessage) => void>();
  private readonly closers = new Set<(code: number) => void>();

  constructor(private readonly socket: Pick<ServerWebSocket<unknown>,
    "readyState" | "getBufferedAmount" | "sendText" | "close"
  >) {}

  get isOpen(): boolean {
    return !this.closed && this.socket.readyState === 1;
  }

  send(message: string): void {
    if (!this.isOpen) return;
    // Disconnect a slow reader instead of retaining unlimited console/log data.
    if (this.socket.getBufferedAmount() + encodeText(message).byteLength > MAX_SOCKET_BUFFER_BYTES) {
      this.close(1013, "Client is not reading output");
      return;
    }
    try {
      if (this.socket.sendText(message) === 0)
        this.close(1013, "Output could not be delivered");
    } catch {
      this.close(1011, "Output connection failed");
    }
  }

  receive(message: SocketMessage): void {
    if (!this.isOpen) return;
    for (const listener of this.messages) {
      if (!this.isOpen) break;
      listener(message);
    }
  }

  close(code: number, reason?: string): void {
    if (this.closed) return;
    // Revoke access and release attached streams immediately, without waiting
    // for the peer to acknowledge the close frame.
    this.finish(code);
    this.socket.close(code, reason);
  }

  finish(code: number): void {
    if (this.closed) return;
    this.closed = true;
    this.closeCode = code;
    this.messages.clear();
    const listeners = [...this.closers];
    this.closers.clear();
    for (const listener of listeners) {
      try {
        listener(code);
      } catch {
        logger.warn("WebSocket cleanup failed", { code });
      }
    }
  }

  onMessage(listener: (message: SocketMessage) => void): void {
    if (!this.closed) this.messages.add(listener);
  }

  onClose(listener: (code: number) => void): void {
    if (this.closed) listener(this.closeCode);
    else this.closers.add(listener);
  }
}
