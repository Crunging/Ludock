// Exercise production with the same restrictions as the example deployment.
export const hardenedContainerArguments = [
  "--read-only", "--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE",
  "--security-opt", "no-new-privileges",
  "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=256m,mode=1777",
];
