import packageJson from "../../package.json";

const currentYear = new Date().getFullYear();

export const APP_CONFIG = {
  name: "SERIES",
  version: packageJson.version,
  copyright: `© ${currentYear}, SERIES.`,
  meta: {
    title: "SERIES — 自主渗透测试控制台",
    description: "LLM 驱动的自主渗透测试系统控制台",
  },
};
