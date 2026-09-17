import type { SocketChannel, SocketMessage } from "../../src/socket-channel.js";

export class SocketFixture implements SocketChannel {
  closed = false;
  closeCode: number | null = null;
  readonly messages: string[] = [];
  readonly sent: Array<{ type: string; data: string }> = [];
  private readonly listeners = new Set<(message: SocketMessage) => void>();
  private readonly closers = new Set<(code: number) => void>();

  get isOpen(): boolean { return !this.closed; }
  onMessage(listener: (message: SocketMessage) => void): void { this.listeners.add(listener); }
  onClose(listener: (code: number) => void): void { this.closers.add(listener); }

  receive(message: SocketMessage): void {
    if (this.closed) return;
    for (const listener of this.listeners) listener(message);
  }

  send(message: string): void {
    if (this.closed) return;
    this.messages.push(message);
    this.sent.push(JSON.parse(message) as { type: string; data: string });
  }

  close(code = 1000): void {
    if (this.closed) return;
    this.closed = true;
    this.closeCode = code;
    this.listeners.clear();
    for (const listener of this.closers) listener(code);
    this.closers.clear();
  }
}
