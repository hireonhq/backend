// Dumps the live Mongo database to JSON files, preserving the original ObjectId
// hex strings as `id`. The seed JSON only carries positional integers, so the
// database is the only authoritative source for the ids already embedded in user
// bookmarks and ML embeddings.
const fs = require("fs");
const path = require("path");
const { MongoClient } = require(path.join(__dirname, "../../../backend-hireonai/node_modules/mongodb"));

const uri = process.env.MONGO_URI || "mongodb://127.0.0.1:27017/hireon";
const out = process.argv[2] || path.join(__dirname, "dump");

const hex = (v) => (v == null ? null : String(v));
const iso = (v) => (v instanceof Date ? v.toISOString() : v ?? null);

async function main() {
  fs.mkdirSync(out, { recursive: true });
  const client = new MongoClient(uri, { directConnection: true });
  await client.connect();
  const db = client.db();

  const write = (name, rows) => {
    fs.writeFileSync(path.join(out, name + ".json"), JSON.stringify(rows, null, 1));
    console.log(`${name.padEnd(20)} ${rows.length}`);
  };

  write("company_industries", (await db.collection("companyindustries").find({}).toArray()).map((d) => ({
    id: hex(d._id), name: d.name, created_at: iso(d.createdAt), updated_at: iso(d.updatedAt),
  })));

  write("companies", (await db.collection("companies").find({}).toArray()).map((d) => ({
    id: hex(d._id), name: d.name, description: d.description, profile_src: d.profileSrc,
    location: d.location, employees_count: d.employeesCount ?? null, industry_id: hex(d.industryId),
    created_at: iso(d.createdAt), updated_at: iso(d.updatedAt),
  })));

  write("job_categories", (await db.collection("jobcategories").find({}).toArray()).map((d) => ({
    id: hex(d._id), name: d.name, created_at: iso(d.createdAt), updated_at: iso(d.updatedAt),
  })));

  write("job_min_experiences", (await db.collection("jobminexperiences").find({}).toArray()).map((d) => ({
    id: hex(d._id), name: d.name, created_at: iso(d.createdAt), updated_at: iso(d.updatedAt),
  })));

  const rawJobs = await db.collection("jobs").find({}).toArray();
  const links = [];
  write("jobs", rawJobs.map((d) => {
    for (const [i, c] of (d.categories || []).entries()) links.push({ job_id: hex(d._id), category_id: hex(c), position: i });
    return {
      id: hex(d._id), url: d.url, company_id: hex(d.companyId), job_position: d.jobPosition,
      employment_type: d.employmentType, working_location_type: d.workingLocationType,
      working_location: d.workingLocation ?? null, min_experience_id: hex(d.minExperienceId),
      min_salary: d.minSalary, max_salary: d.maxSalary,
      job_desc_list: d.jobDescList || [], job_qualifications_list: d.jobQualificationsList || [],
      created_at: iso(d.createdAt), updated_at: iso(d.updatedAt),
    };
  }));
  write("job_category_links", links);

  write("users", (await db.collection("users").find({}).toArray()).map((d) => ({
    id: hex(d._id), username: d.username, email: d.email, supabase_id: d.supabaseId ?? null,
    password: d.password ?? null, photo_url: d.photoUrl ?? null, role: d.role || "seeker",
    oauth_provider: d.oauthProvider ?? null, verified_at: iso(d.verifiedAt),
    created_at: iso(d.createdAt), updated_at: iso(d.updatedAt),
  })));

  const rawProfiles = await db.collection("profiles").find({}).toArray();
  const bookmarks = [];
  write("profiles", rawProfiles.map((d) => {
    for (const j of d.bookmarkJobs || []) bookmarks.push({ profile_id: hex(d._id), job_id: hex(j) });
    return {
      id: hex(d._id), user_id: hex(d.userId), fullname: d.fullname, phone: d.phone ?? null,
      domicile: d.domicile ?? null, last_education: d.lastEducation ?? null,
      photo_url: d.photoUrl ?? null, portfolio_url: d.portfolioUrl ?? null, cv_url: d.cvUrl ?? null,
      // Any pre-existing cvUrl predates async processing, so its embedding state is
      // unknown; mark it ready to preserve today's synchronous behaviour.
      cv_status: d.cvUrl ? "ready" : "none",
      tag_preferences: d.tagPreferences || [],
      created_at: iso(d.createdAt), updated_at: iso(d.updatedAt),
    };
  }));
  write("bookmark_jobs", bookmarks);

  write("analysis_results", (await db.collection("analysisresults").find({}).toArray()).map((d) => ({
    id: hex(d._id), user_id: hex(d.userId), job_id: hex(d.jobId),
    cv_relevance_score: d.cvRelevanceScore, explanation: d.explanation,
    skil_identification_dict: d.skilIdentificationDict || {}, suggestions: d.suggestions || [],
    improvements: d.improvements || [], created_at: iso(d.createdAt), updated_at: iso(d.updatedAt),
  })));

  await client.close();
}

main().catch((e) => { console.error(e); process.exit(1); });
