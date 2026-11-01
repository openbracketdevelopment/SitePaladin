use async_trait::async_trait;
use sqlx::{Row, SqlitePool};
use time::OffsetDateTime;
use tower_sessions::{
    SessionStore,
    session::{Id, Record},
    session_store::{Error, Result},
};

#[derive(Clone, Debug)]
pub struct SqliteSessionStore {
    pool: SqlitePool,
}

impl SqliteSessionStore {
    pub fn new(pool: SqlitePool) -> Self {
        Self { pool }
    }

    pub async fn delete_expired(&self) -> Result<()> {
        sqlx::query("DELETE FROM sessions WHERE expires_at <= ?")
            .bind(OffsetDateTime::now_utc().unix_timestamp())
            .execute(&self.pool)
            .await
            .map_err(backend_error)?;
        Ok(())
    }
}

#[async_trait]
impl SessionStore for SqliteSessionStore {
    async fn create(&self, record: &mut Record) -> Result<()> {
        let data = serde_json::to_string(&record.data)
            .map_err(|error| Error::Encode(error.to_string()))?;
        loop {
            let result = sqlx::query(
                "INSERT INTO sessions(id,data,expires_at) VALUES(?,?,?) ON CONFLICT(id) DO NOTHING",
            )
            .bind(record.id.to_string())
            .bind(&data)
            .bind(record.expiry_date.unix_timestamp())
            .execute(&self.pool)
            .await
            .map_err(backend_error)?;
            if result.rows_affected() == 1 {
                return Ok(());
            }
            record.id = Id::default();
        }
    }

    async fn save(&self, record: &Record) -> Result<()> {
        let data = serde_json::to_string(&record.data)
            .map_err(|error| Error::Encode(error.to_string()))?;
        sqlx::query("UPDATE sessions SET data=?,expires_at=? WHERE id=?")
            .bind(data)
            .bind(record.expiry_date.unix_timestamp())
            .bind(record.id.to_string())
            .execute(&self.pool)
            .await
            .map_err(backend_error)?;
        // A concurrent logout must not allow an in-flight request to recreate the session.
        Ok(())
    }

    async fn load(&self, id: &Id) -> Result<Option<Record>> {
        let row = sqlx::query("SELECT data,expires_at FROM sessions WHERE id=? AND expires_at > ?")
            .bind(id.to_string())
            .bind(OffsetDateTime::now_utc().unix_timestamp())
            .fetch_optional(&self.pool)
            .await
            .map_err(backend_error)?;
        row.map(|row| {
            let data: String = row.try_get("data").map_err(backend_error)?;
            let expires_at: i64 = row.try_get("expires_at").map_err(backend_error)?;
            Ok(Record {
                id: *id,
                data: serde_json::from_str(&data)
                    .map_err(|error| Error::Decode(error.to_string()))?,
                expiry_date: OffsetDateTime::from_unix_timestamp(expires_at)
                    .map_err(|error| Error::Decode(error.to_string()))?,
            })
        })
        .transpose()
    }

    async fn delete(&self, id: &Id) -> Result<()> {
        sqlx::query("DELETE FROM sessions WHERE id=?")
            .bind(id.to_string())
            .execute(&self.pool)
            .await
            .map_err(backend_error)?;
        Ok(())
    }
}

fn backend_error(error: sqlx::Error) -> Error {
    tracing::error!(%error, "SQLite session store failed");
    Error::Backend(error.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use tower_sessions::{Expiry, Session};

    async fn store() -> SqliteSessionStore {
        let pool = SqlitePool::connect("sqlite::memory:").await.unwrap();
        sqlx::migrate!("./migrations").run(&pool).await.unwrap();
        SqliteSessionStore::new(pool)
    }

    #[tokio::test]
    async fn sessions_survive_store_recreation_and_logout_revokes_them() {
        let store = store().await;
        let session = Session::new(
            None,
            Arc::new(store.clone()),
            Some(Expiry::OnInactivity(time::Duration::hours(12))),
        );
        session.insert("authenticated", true).await.unwrap();
        session.insert("csrf_token", "test-token").await.unwrap();
        session.save().await.unwrap();
        let id = session.id().unwrap();
        let restarted = SqliteSessionStore::new(store.pool.clone());
        let restored = Session::new(Some(id), Arc::new(restarted.clone()), None);
        assert_eq!(
            restored.get::<bool>("authenticated").await.unwrap(),
            Some(true)
        );
        assert_eq!(
            restored
                .get::<String>("csrf_token")
                .await
                .unwrap()
                .as_deref(),
            Some("test-token")
        );
        let stale_record = restarted.load(&id).await.unwrap().unwrap();
        restored.flush().await.unwrap();
        restarted.save(&stale_record).await.unwrap();
        assert!(restarted.load(&id).await.unwrap().is_none());
    }

    #[tokio::test]
    async fn sessions_persist_after_closing_and_reopening_the_database() {
        let path =
            std::env::temp_dir().join(format!("sitepaladin-session-{}.db", uuid::Uuid::new_v4()));
        let options = sqlx::sqlite::SqliteConnectOptions::new()
            .filename(&path)
            .create_if_missing(true);
        let pool = SqlitePool::connect_with(options.clone()).await.unwrap();
        sqlx::migrate!("./migrations").run(&pool).await.unwrap();
        let store = SqliteSessionStore::new(pool.clone());
        let session = Session::new(
            None,
            Arc::new(store),
            Some(Expiry::OnInactivity(time::Duration::hours(12))),
        );
        session.insert("authenticated", true).await.unwrap();
        session.save().await.unwrap();
        let old_id = session.id().unwrap();
        session.cycle_id().await.unwrap();
        session.save().await.unwrap();
        let id = session.id().unwrap();
        assert_ne!(old_id, id);
        drop(session);
        pool.close().await;

        let pool = SqlitePool::connect_with(options).await.unwrap();
        let store = SqliteSessionStore::new(pool.clone());
        assert!(store.load(&old_id).await.unwrap().is_none());
        let restored = Session::new(Some(id), Arc::new(store), None);
        assert_eq!(
            restored.get::<bool>("authenticated").await.unwrap(),
            Some(true)
        );
        drop(restored);
        pool.close().await;
        std::fs::remove_file(path).unwrap();
    }

    #[tokio::test]
    async fn malformed_session_data_returns_an_error_instead_of_authenticating() {
        let store = store().await;
        let id = Id::default();
        sqlx::query("INSERT INTO sessions(id,data,expires_at) VALUES(?,?,?)")
            .bind(id.to_string())
            .bind("invalid json")
            .bind((OffsetDateTime::now_utc() + time::Duration::hours(12)).unix_timestamp())
            .execute(&store.pool)
            .await
            .unwrap();
        assert!(matches!(store.load(&id).await, Err(Error::Decode(_))));
    }

    #[tokio::test]
    async fn expired_sessions_are_rejected_and_cleaned_up() {
        let store = store().await;
        let mut record = Record {
            id: Id::default(),
            data: Default::default(),
            expiry_date: OffsetDateTime::now_utc() - time::Duration::seconds(1),
        };
        store.create(&mut record).await.unwrap();
        assert!(store.load(&record.id).await.unwrap().is_none());
        store.delete_expired().await.unwrap();
        let count: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM sessions")
            .fetch_one(&store.pool)
            .await
            .unwrap();
        assert_eq!(count, 0);
    }

    #[tokio::test]
    async fn id_collisions_do_not_overwrite_existing_sessions() {
        let store = store().await;
        let mut first = Record {
            id: Id::default(),
            data: Default::default(),
            expiry_date: OffsetDateTime::now_utc() + time::Duration::hours(12),
        };
        store.create(&mut first).await.unwrap();
        let mut second = first.clone();
        second.data.insert("other".into(), serde_json::json!(true));
        store.create(&mut second).await.unwrap();
        assert_ne!(first.id, second.id);
        assert!(
            store
                .load(&first.id)
                .await
                .unwrap()
                .unwrap()
                .data
                .is_empty()
        );
    }
}
