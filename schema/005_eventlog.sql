CREATE TABLE eventlog(
       id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
       language text NOT NULL,
       entry text NOT NULL,
       deleted boolean NOT NULL DEFAULT false,
       created timestamptz NOT NULL DEFAULT now()
);

---- create above / drop below ----

DROP TABLE eventlog;
