// @ts-check
import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";

const repo = "https://github.com/riptideslabs/waterwheel";

// On GitHub Pages the workflow passes the origin and base path from
// actions/configure-pages; locally the site is served from the root.
export default defineConfig({
    site: process.env.SITE_URL || "https://riptideslabs.github.io",
    base: process.env.BASE_PATH || "/",
    integrations: [
        starlight({
            title: "Waterwheel",
            description:
                "Waterwheel exchanges a pod's Kubernetes service account token for short-lived API tokens, so pods call Anthropic, OpenAI and other APIs without holding a key.",
            logo: { src: "./src/assets/waterwheel.svg" },
            favicon: "/favicon.svg",
            customCss: ["@fontsource-variable/inter", "./src/styles/custom.css"],
            components: { Hero: "./src/components/Hero.astro" },
            expressiveCode: {
                styleOverrides: {
                    borderRadius: "0.75rem",
                    borderColor: "var(--sl-color-gray-5)",
                    frames: { frameBoxShadowCssValue: "none" },
                },
            },
            social: [{ icon: "github", label: "GitHub", href: repo }],
            editLink: { baseUrl: `${repo}/edit/main/site/` },
        }),
    ],
});
