// Check before loading Husky because production installs may omit it.
if (process.env.HUSKY !== "0" && process.env.NODE_ENV !== "production") {
  const { default: install } = await import("husky");
  const message = install();
  if (message) console.log(message);
}
