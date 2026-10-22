// Builds cmd/dashboard/web/static/app.css (run `make css` after changing
// classes in templates or Go code). The dashboard used to compile Tailwind in
// the browser from cdn.tailwindcss.com; serving prebuilt CSS removes that
// third-party script from a page that can stop and upgrade containers.
module.exports = {
  content: [
    './cmd/dashboard/web/templates/**/*.html',
    // Go template helpers that return class names (status colours, badges).
    // Only this file: scanning every Go file picks up ordinary words in code
    // and comments ("inline", "hidden"…) as classes, so any comment edit
    // would change app.css and fail CI's freshness check.
    './internal/dashboard/handlers.go',
  ],
  theme: { extend: {} },
  plugins: [],
}
