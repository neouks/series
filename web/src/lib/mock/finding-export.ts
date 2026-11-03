import type { Finding } from "../types";

// Small uncompressed ZIP writer for demo downloads only; no runtime dependency
// or report generation. Production exports continue to use the backend.
function zip(files: { name: string; text: string }[]) {
  const encoder = new TextEncoder();
  const parts: Uint8Array[] = [],
    central: Uint8Array[] = [];
  let offset = 0,
    centralSize = 0;
  for (const file of files) {
    const name = encoder.encode(file.name),
      data = encoder.encode(file.text);
    let crc = 0xffffffff;
    for (const byte of data) {
      crc ^= byte;
      for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ (crc & 1 ? 0xedb88320 : 0);
    }
    crc = (crc ^ 0xffffffff) >>> 0;
    const local = new Uint8Array(30 + name.length),
      head = new DataView(local.buffer);
    head.setUint32(0, 0x04034b50, true);
    head.setUint16(4, 20, true);
    head.setUint16(6, 0x800, true);
    head.setUint32(14, crc, true);
    head.setUint32(18, data.length, true);
    head.setUint32(22, data.length, true);
    head.setUint16(26, name.length, true);
    local.set(name, 30);
    const entry = new Uint8Array(46 + name.length),
      info = new DataView(entry.buffer);
    info.setUint32(0, 0x02014b50, true);
    info.setUint16(4, 20, true);
    info.setUint16(6, 20, true);
    info.setUint16(8, 0x800, true);
    info.setUint32(16, crc, true);
    info.setUint32(20, data.length, true);
    info.setUint32(24, data.length, true);
    info.setUint16(28, name.length, true);
    info.setUint32(42, offset, true);
    entry.set(name, 46);
    parts.push(local, data);
    central.push(entry);
    offset += local.length + data.length;
    centralSize += entry.length;
  }
  const end = new Uint8Array(22),
    view = new DataView(end.buffer);
  view.setUint32(0, 0x06054b50, true);
  view.setUint16(8, files.length, true);
  view.setUint16(10, files.length, true);
  view.setUint32(12, centralSize, true);
  view.setUint32(16, offset, true);
  const result = new Uint8Array(offset + centralSize + end.length);
  let position = 0;
  for (const part of [...parts, ...central, end]) {
    result.set(part, position);
    position += part.length;
  }
  return result;
}

export function mockFindingExport(items: Finding[], format: string) {
  const title = (f: Finding) => {
    if (f.name) return f.name;
    return f.vulnclass;
  };
  const reportTitle = (f: Finding) => {
    for (const line of (f.report ?? "").split("\n")) {
      const trimmed = line.trim();
      const prefix = "**漏洞名称：**";
      if (trimmed.startsWith(prefix)) {
        const value = trimmed.slice(prefix.length).trim();
        if (value) return value;
      }
    }
    return title(f);
  };
  const reportOnly = (f: Finding) => f.report ?? "";
  if (format === "md-report-zip" || format === "md-zip")
    return {
      filename: "findings-report-demo.zip",
      bytes: zip(
        items.filter((f) => reportOnly(f).trim()).map((f) => ({ name: `finding-${f.id}.md`, text: reportOnly(f) })),
      ),
    };
  let text: string, extension: string;
  if (format === "json") {
    const clean = items.map((f) => {
      const copy = { ...f };
      delete copy.report_detail;
      return copy;
    });
    text = JSON.stringify(clean, null, 2);
    extension = "json";
  } else if (format === "csv") {
    const field = (value: string) => `"${value.replaceAll('"', '""')}"`;
    const header = ["ID", "漏洞名称", "漏洞等级", "漏洞类型", "状态", "所属任务", "发现时间", "提交报告"];
    text = `\uFEFF${[
      header,
      ...items.map((f) => [
        f.id,
        reportTitle(f),
        f.severity,
        f.vulnclass,
        f.status,
        f.task_description ?? "",
        f.ts,
        reportOnly(f),
      ]),
    ]
      .map((row) => row.map((value) => field(String(value))).join(","))
      .join("\r\n")}`;
    extension = "csv";
  } else if (format === "md-report" || format === "md-single") {
    text = items
      .map(reportOnly)
      .filter((s) => s.trim())
      .join("\n---\n\n");
    extension = "md";
  } else throw new Error("invalid export format");
  return { filename: `findings-demo.${extension}`, bytes: new TextEncoder().encode(text) };
}
