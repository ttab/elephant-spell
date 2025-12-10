--
-- PostgreSQL database dump
--

-- Dumped from database version 17.4 (Debian 17.4-1.pgdg120+2)
-- Dumped by pg_dump version 17.4 (Debian 17.4-1.pgdg120+2)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: entry_level; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.entry_level AS ENUM (
    'suggestion',
    'error'
);


SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: entry; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.entry (
    language text NOT NULL,
    entry text NOT NULL,
    status text NOT NULL,
    description text NOT NULL,
    common_mistakes text[],
    level public.entry_level DEFAULT 'error'::public.entry_level NOT NULL,
    data jsonb
);


--
-- Name: schema_version; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.schema_version (
    version integer NOT NULL
);


--
-- Name: entry entry_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entry
    ADD CONSTRAINT entry_pkey PRIMARY KEY (language, entry);


--
-- Name: idx_entry_pattern_ops; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_entry_pattern_ops ON public.entry USING btree (entry varchar_pattern_ops);


--
-- PostgreSQL database dump complete
--

