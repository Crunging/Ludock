/** Private transport contracts. Helpers still validate paths and file metadata. */
export interface FileHelperRequest {
  operation:
    | "check"
    | "backup"
    | "list"
    | "stat"
    | "mkdir"
    | "delete"
    | "rename"
    | "upload"
    | "upload-cleanup"
    | "download";
  root: string;
  path?: string;
  blocked?: string[];
  destination?: string;
  size?: number;
  uploadId?: string;
}

export interface RestoreRequest {
  root: string;
  stage: string;
  operation:
    | "space"
    | "stage"
    | "cleanup"
    | "moveOld"
    | "moveNew"
    | "rollbackClean"
    | "rollbackOld";
}

export interface RestoreRecord {
  name: string;
  type: "file" | "directory";
  size: number;
  uid: number;
  gid: number;
  mode: number;
  mtime: number;
}
