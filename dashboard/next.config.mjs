/** @type {import('next').NextConfig} */
const nextConfig = {
  output: "standalone",
  // The dashboard runs as a separate origin from the API; talk to the API
  // via NEXT_PUBLIC_API_URL (set in compose to http://localhost:8080).
  reactStrictMode: true,
};

export default nextConfig;
