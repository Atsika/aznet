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
      social: [
        {
          icon: "github",
          label: "GitHub",
          href: "https://github.com/atsika/aznet",
        },
      ],
      sidebar: [
        {
          label: "Start Here",
          items: [
            { label: "Introduction", slug: "index" },
            { label: "Getting Started", slug: "getting-started" },
          ],
        },
        {
          label: "Core Concepts",
          items: [
            { label: "Architecture", slug: "core-concepts/architecture" },
            { label: "Security", slug: "core-concepts/security" },
          ],
        },
        {
          label: "Drivers",
          items: [
            { label: "Overview", slug: "drivers/overview" },
            { label: "Azure Blob Storage", slug: "drivers/azblob" },
            { label: "Azure Queue Storage", slug: "drivers/azqueue" },
            { label: "Azure Table Storage", slug: "drivers/aztable" },
            { label: "Cost Analysis", slug: "drivers/cost" },
            { label: "Performance Analysis", slug: "drivers/performance" },
          ],
        },
        {
          label: "Tools",
          items: [{ label: "azurl", slug: "tools/azurl" }],
        },
        {
          label: "Guides",
          items: [
            { label: "Local Development", slug: "guides/azurite" },
            { label: "Developing a Driver", slug: "guides/developing-drivers" },
            { label: "Examples", slug: "guides/examples" },
          ],
        },
        {
          label: "Reference",
          items: [
            { label: "Core API", slug: "reference/api" },
            { label: "Configuration Options", slug: "reference/options" },
            { label: "Metrics & Monitoring", slug: "reference/metrics" },
          ],
        },
      ],
    }),
  ],
});
