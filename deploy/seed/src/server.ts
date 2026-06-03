import { serve } from "@hono/node-server";
import { Hono } from "hono";
import { total } from "./price.ts";

const app = new Hono();
app.get("/", (c) => c.html(`<h1>Shop</h1><p>Total: ${total([1999, 501])}</p>`));
serve({ fetch: app.fetch, port: Number(process.env.PORT), hostname: process.env.HOST });
