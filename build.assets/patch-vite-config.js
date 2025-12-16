/**
 * Patch script to remove problematic vite plugins for Docker builds
 * These plugins cause issues with Node.js ESM compatibility in the Docker build environment
 */
const fs = require('fs');

const configPath = 'web/packages/build/vite/config.ts';
let content = fs.readFileSync(configPath, 'utf8');

// Remove visualizer import
content = content.replace(
  "import { visualizer } from 'rollup-plugin-visualizer';\n",
  ''
);

// Remove visualizer if block
content = content.replace(
  `
    if (process.env.VITE_ANALYZE_BUNDLE) {
      config.plugins.push(visualizer());
    }`,
  ''
);

// Remove compression import
content = content.replace(
  "import compression from 'vite-plugin-compression';\n",
  ''
);

// Remove the compression if block
const compressionBlock = `
      if (!process.env.VITE_DISABLE_COMPRESSION) {
        config.plugins.push(
          compression({
            algorithm: 'brotliCompress',
            deleteOriginFile: true,
            filter: /\\.(js|svg|wasm)$/,
            threshold: 1024 * 10, // 10KB
            verbose: false,
          })
        );
      }`;
content = content.replace(compressionBlock, '');

// Remove wasm import
content = content.replace("import wasm from 'vite-plugin-wasm';\n", '');

// Remove wasm() from plugins array
content = content.replace('        wasm(),\n', '');

fs.writeFileSync(configPath, content);
console.log('Patched vite config successfully');
