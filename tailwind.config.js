// Builds cmd/dashboard/web/static/app.css (run `make css` after changing
// classes in templates or Go code). The dashboard used to compile Tailwind in
// the browser from cdn.tailwindcss.com; serving prebuilt CSS removes that
// third-party script from a page that can stop and upgrade containers.
module.exports = {
  content: [
    './cmd/dashboard/web/templates/**/*.html',
    // Go template helpers return class names (status colours, badges).
    './internal/dashboard/**/*.go',
  ],
  theme: { extend: {} },
  plugins: [],
}
