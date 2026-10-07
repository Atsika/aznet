// @ts-check
import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";
import mermaid from "astro-mermaid";

// https://astro.build/config
export default defineConfig({
  integrations: [
    mermaid({
      iconPacks: [
        {
          name: "logos",
          loader: () =>
            fetch("https://unpkg.com/@iconify-json/logos@1/icons.json").then(
              (res) => res.json(),
            ),
        },
        {
          name: "iconoir",
          loader: () =>
            fetch("https://unpkg.com/@iconify-json/iconoir@1/icons.json").then(
              (res) => res.json(),
            ),
        },
      ],
    }),
    starlight({
      title: "aznet",
      customCss: ["./src/styles/docs.css"],
      social: [
        {
          icon: "github",
          label: "GitHub",
          href: "https://github.com/atsika/aznet",
        },
      ],
      sidebar: [
        {
          label: "Start",
          items: [
            { label: "Overview", slug: "index" },
            { label: "Getting started", slug: "getting-started" },
            { label: "Examples", slug: "guides/examples" },
          ],
        },
        {
          label: "Use aznet",
          items: [
            { label: "API reference", slug: "reference/api" },
            { label: "Configuration", slug: "reference/options" },
            { label: "Connection URLs", slug: "tools/azurl" },
            { label: "Security & authorization", slug: "core-concepts/security" },
          ],
        },
        {
          label: "Storage drivers",
          collapsed: true,
          items: [
            { label: "Choose a driver", slug: "drivers/overview" },
            { label: "Blob", slug: "drivers/azblob" },
            { label: "Queue", slug: "drivers/azqueue" },
            { label: "Table", slug: "drivers/aztable" },
          ],
        },
        {
          label: "Develop & operate",
          collapsed: true,
          items: [
            { label: "Local emulator", slug: "guides/azurite" },
            { label: "Architecture", slug: "core-concepts/architecture" },
            { label: "Develop a driver", slug: "guides/developing-drivers" },
            { label: "Metrics", slug: "reference/metrics" },
            { label: "Measure performance", slug: "drivers/performance" },
            { label: "Estimate cost", slug: "drivers/cost" },
            { label: "Validation & migration", slug: "guides/validation" },
          ],
        },
      ],
    }),
  ],
});
